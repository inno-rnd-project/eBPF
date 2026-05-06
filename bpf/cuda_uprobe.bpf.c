// gpuobs CUDA uprobe BPF 프로그램.
//
// libcuda.so.1 의 14개 심볼에 uprobe 를 걸어 (pid, kind, bytes) 이벤트를 ringbuf 로 emit 한다.
// 심볼 선택 근거:
//   * cuLaunchKernel / cuLaunchKernelEx / cuLaunchCooperativeKernel
//     - CUDA Driver API 의 모든 커널 런치 경로. PyTorch / TF 가 둘 다 통과한다.
//   * cuMemcpyHtoD_v2 / cuMemcpyHtoDAsync_v2 / cuMemcpyDtoH_v2 / cuMemcpyDtoHAsync_v2
//     - 방향이 명확한 host↔device 메모리 전송 경로. ByteCount(rdx) 를 그대로 발행한다.
//   * cuMemcpyDtoD_v2 / cuMemcpyDtoDAsync_v2
//     - 방향이 항상 device→device 인 경로. ByteCount(rdx) 를 그대로 발행한다.
//   * cuMemcpy2D_v2 / cuMemcpy2DAsync_v2 / cuMemcpy3D_v2 / cuMemcpy3DAsync_v2
//     - 구조체 인자 (CUDA_MEMCPY2D / 3D) 로 방향이 결정되는 경로. bpf_probe_read_user 로
//       srcMemoryType / dstMemoryType 필드를 읽어 명시 방향 (h2d / d2h / dtod) 으로 분류하고,
//       UNIFIED / ARRAY / HOST→HOST 등 방향 불명확 케이스는 UNKNOWN_DIR kind 로 emit 한다.
//   * cuMemcpy
//     - UVA 경로. dst / src 모두 CUdeviceptr 라 BPF 단에서 host/device 구분 불가하므로 항상
//       UNKNOWN_DIR 로 emit 한다.
//
// CUDA Runtime API (cudaMemcpy*, cudaLaunchKernel) 는 본 PR 의 후속 commit 에서 추가된다.
//
// userspace 측 PID→GPU 귀속은 NVML GetComputeRunningProcesses 캐시로 수행하므로
// 본 BPF 프로그램은 GPU 식별자를 모른다. 이벤트는 prog→user 의 thin pipe 역할만 한다.

#include "vmlinux.h"

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

enum cuda_event_kind {
    CUDA_EVENT_KERNEL_LAUNCH = 1,
    CUDA_EVENT_H2D           = 2,
    CUDA_EVENT_D2H           = 3,
    CUDA_EVENT_DTOD          = 4,
    /* UNKNOWN_DIR 은 BPF 단에서 방향 판정이 불가능한 케이스 — UVA cuMemcpy 와 cuMemcpy2D/3D 의
     * ARRAY / UNIFIED / HOST→HOST 분기에서 사용된다. */
    CUDA_EVENT_UNKNOWN_DIR   = 5,
};

/* CUDA_MEMCPY2D / CUDA_MEMCPY3D 구조체의 byte offset (x86_64 linux ABI).
 * cuda.h 헤더의 정의를 참조해 패딩 / 정렬을 포함한 절대 위치를 직접 명시한다 — 헤더 변경 시 본 값들도
 * 함께 갱신해야 한다. CUmemorytype 은 unsigned int (4 bytes), 그 외 필드는 size_t / pointer (8 bytes) 다.
 */
#define MEMCPY2D_SRC_MEMTYPE_OFF  16
#define MEMCPY2D_DST_MEMTYPE_OFF  72
#define MEMCPY2D_WIDTH_BYTES_OFF  112
#define MEMCPY2D_HEIGHT_OFF       120

#define MEMCPY3D_SRC_MEMTYPE_OFF  32
#define MEMCPY3D_DST_MEMTYPE_OFF  120
#define MEMCPY3D_WIDTH_BYTES_OFF  176
#define MEMCPY3D_HEIGHT_OFF       184
#define MEMCPY3D_DEPTH_OFF        192

/* CUmemorytype enum 값. cuda.h 의 CU_MEMORYTYPE_* 와 1:1. */
#define CU_MEMORYTYPE_HOST    1
#define CU_MEMORYTYPE_DEVICE  2
#define CU_MEMORYTYPE_ARRAY   3
#define CU_MEMORYTYPE_UNIFIED 4

// userspace 의 Go 측 구조체와 layout 이 정확히 일치해야 한다 (binary.NativeEndian Read).
// 32 bytes 고정 — 8(ts) + 8(bytes) + 4(pid) + 4(tid) + 1(kind) + 7(pad).
struct cuda_event {
    __u64 ts_ns;
    __u64 bytes;     /* memcpy 시 ByteCount; kernel launch 시 0 */
    __u32 pid;
    __u32 tid;
    __u8  kind;
    __u8  pad[7];
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 22);    /* 4 MiB */
} cuda_events SEC(".maps");

/* cilium/ebpf 의 ringbuf API 는 perf 와 달리 lost sample 카운터를 노출하지 않으므로,
 * bpf_ringbuf_reserve 가 NULL 을 돌려준 (즉 ringbuf 가 가득 차서 record 를 못 잡은) 케이스를
 * BPF 측 percpu 카운터로 직접 누적한다. userspace 가 주기적으로 read + sum 해서
 * gpuobs_cuda_events_lost_total 카운터에 delta 만 add 한다.
 *
 * percpu 로 잡아 producer 측 atomic 비용을 0 으로 둔다 (각 CPU 의 자기 슬롯만 갱신).
 */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u64);
} cuda_dropped SEC(".maps");

static __always_inline void inc_dropped(void)
{
    __u32 key = 0;
    __u64 *v = bpf_map_lookup_elem(&cuda_dropped, &key);
    if (v)
        (*v)++;        /* percpu 슬롯이라 비-원자 증가로 충분하다 */
}

static __always_inline void emit_event(__u8 kind, __u64 bytes)
{
    struct cuda_event *e;
    __u64 pid_tgid;

    e = bpf_ringbuf_reserve(&cuda_events, sizeof(*e), 0);
    if (!e) {
        inc_dropped();
        return;
    }

    pid_tgid = bpf_get_current_pid_tgid();
    e->ts_ns = bpf_ktime_get_ns();
    e->bytes = bytes;
    e->pid   = pid_tgid >> 32;
    e->tid   = (__u32)pid_tgid;
    e->kind  = kind;
    e->pad[0] = 0;
    e->pad[1] = 0;
    e->pad[2] = 0;
    e->pad[3] = 0;
    e->pad[4] = 0;
    e->pad[5] = 0;
    e->pad[6] = 0;

    bpf_ringbuf_submit(e, 0);
}

/* kernel launch 3종은 인자에서 추출할 정보가 없어 단순 카운트 이벤트만 발행한다. */
SEC("uprobe/cuLaunchKernel")
int BPF_UPROBE(handle_cu_launch_kernel)
{
    emit_event(CUDA_EVENT_KERNEL_LAUNCH, 0);
    return 0;
}

SEC("uprobe/cuLaunchKernelEx")
int BPF_UPROBE(handle_cu_launch_kernel_ex)
{
    emit_event(CUDA_EVENT_KERNEL_LAUNCH, 0);
    return 0;
}

SEC("uprobe/cuLaunchCooperativeKernel")
int BPF_UPROBE(handle_cu_launch_cooperative_kernel)
{
    emit_event(CUDA_EVENT_KERNEL_LAUNCH, 0);
    return 0;
}

/*
 * cuMemcpyHtoD_v2(CUdeviceptr dstDevice, const void *srcHost, size_t ByteCount)
 * cuMemcpyHtoDAsync_v2(..., CUstream hStream)
 * cuMemcpyDtoH_v2(void *dstHost, CUdeviceptr srcDevice, size_t ByteCount)
 * cuMemcpyDtoHAsync_v2(..., CUstream hStream)
 *
 * 모두 ByteCount 가 3번째 인자 (x86_64 SysV: rdx, PT_REGS_PARM3) 에 위치.
 */
SEC("uprobe/cuMemcpyHtoD_v2")
int BPF_UPROBE(handle_cu_memcpy_htod)
{
    __u64 bytes = (__u64)PT_REGS_PARM3(ctx);
    emit_event(CUDA_EVENT_H2D, bytes);
    return 0;
}

SEC("uprobe/cuMemcpyHtoDAsync_v2")
int BPF_UPROBE(handle_cu_memcpy_htod_async)
{
    __u64 bytes = (__u64)PT_REGS_PARM3(ctx);
    emit_event(CUDA_EVENT_H2D, bytes);
    return 0;
}

SEC("uprobe/cuMemcpyDtoH_v2")
int BPF_UPROBE(handle_cu_memcpy_dtoh)
{
    __u64 bytes = (__u64)PT_REGS_PARM3(ctx);
    emit_event(CUDA_EVENT_D2H, bytes);
    return 0;
}

SEC("uprobe/cuMemcpyDtoHAsync_v2")
int BPF_UPROBE(handle_cu_memcpy_dtoh_async)
{
    __u64 bytes = (__u64)PT_REGS_PARM3(ctx);
    emit_event(CUDA_EVENT_D2H, bytes);
    return 0;
}

/*
 * cuMemcpyDtoD_v2(CUdeviceptr dstDevice, CUdeviceptr srcDevice, size_t ByteCount)
 * cuMemcpyDtoDAsync_v2(..., CUstream hStream)
 *
 * 두 심볼 모두 ByteCount 가 3번째 인자 (x86_64 SysV: rdx, PT_REGS_PARM3) 에 위치하며 방향이 항상 dtod 다.
 * 단일 GPU 안의 device-to-device copy 와 cudaMemcpyDeviceToDevice 의 driver API 백엔드를 모두 커버한다.
 */
SEC("uprobe/cuMemcpyDtoD_v2")
int BPF_UPROBE(handle_cu_memcpy_dtod)
{
    __u64 bytes = (__u64)PT_REGS_PARM3(ctx);
    emit_event(CUDA_EVENT_DTOD, bytes);
    return 0;
}

SEC("uprobe/cuMemcpyDtoDAsync_v2")
int BPF_UPROBE(handle_cu_memcpy_dtod_async)
{
    __u64 bytes = (__u64)PT_REGS_PARM3(ctx);
    emit_event(CUDA_EVENT_DTOD, bytes);
    return 0;
}

/* dir_from_memtype 는 (src, dst) memtype 쌍을 cuda_event_kind 로 변환한다.
 * 명시 방향 (h2d / d2h / dtod) 외의 모든 케이스 (ARRAY / UNIFIED / HOST→HOST 등) 는
 * UNKNOWN_DIR 을 반환해 metric 측에서 별도 카운터로 누적된다.
 */
static __always_inline __u8 dir_from_memtype(__u32 src, __u32 dst)
{
    if (src == CU_MEMORYTYPE_HOST && dst == CU_MEMORYTYPE_DEVICE)
        return CUDA_EVENT_H2D;
    if (src == CU_MEMORYTYPE_DEVICE && dst == CU_MEMORYTYPE_HOST)
        return CUDA_EVENT_D2H;
    if (src == CU_MEMORYTYPE_DEVICE && dst == CU_MEMORYTYPE_DEVICE)
        return CUDA_EVENT_DTOD;
    return CUDA_EVENT_UNKNOWN_DIR;
}

/* emit_memcpy2d / emit_memcpy3d 는 user-space 의 CUDA_MEMCPY2D / 3D 구조체 포인터에서 srcMemoryType /
 * dstMemoryType / WidthInBytes / Height (3D 는 Depth 까지) 만 bpf_probe_read_user 로 읽어 명시 방향이면
 * 총 byte 크기와 함께 emit 한다. pCopy 가 NULL 이거나 read 실패 시 0 으로 남아 자연 skip 된다.
 */
static __always_inline void emit_memcpy2d(__u64 pCopy)
{
    if (!pCopy)
        return;
    __u32 src_mt = 0, dst_mt = 0;
    bpf_probe_read_user(&src_mt, sizeof(src_mt), (const void *)(pCopy + MEMCPY2D_SRC_MEMTYPE_OFF));
    bpf_probe_read_user(&dst_mt, sizeof(dst_mt), (const void *)(pCopy + MEMCPY2D_DST_MEMTYPE_OFF));
    __u8 kind = dir_from_memtype(src_mt, dst_mt);
    __u64 width = 0, height = 0;
    bpf_probe_read_user(&width, sizeof(width), (const void *)(pCopy + MEMCPY2D_WIDTH_BYTES_OFF));
    bpf_probe_read_user(&height, sizeof(height), (const void *)(pCopy + MEMCPY2D_HEIGHT_OFF));
    emit_event(kind, width * height);
}

static __always_inline void emit_memcpy3d(__u64 pCopy)
{
    if (!pCopy)
        return;
    __u32 src_mt = 0, dst_mt = 0;
    bpf_probe_read_user(&src_mt, sizeof(src_mt), (const void *)(pCopy + MEMCPY3D_SRC_MEMTYPE_OFF));
    bpf_probe_read_user(&dst_mt, sizeof(dst_mt), (const void *)(pCopy + MEMCPY3D_DST_MEMTYPE_OFF));
    __u8 kind = dir_from_memtype(src_mt, dst_mt);
    __u64 width = 0, height = 0, depth = 0;
    bpf_probe_read_user(&width, sizeof(width), (const void *)(pCopy + MEMCPY3D_WIDTH_BYTES_OFF));
    bpf_probe_read_user(&height, sizeof(height), (const void *)(pCopy + MEMCPY3D_HEIGHT_OFF));
    bpf_probe_read_user(&depth, sizeof(depth), (const void *)(pCopy + MEMCPY3D_DEPTH_OFF));
    emit_event(kind, width * height * depth);
}

/*
 * cuMemcpy2D_v2(const CUDA_MEMCPY2D *pCopy)
 * cuMemcpy2DAsync_v2(const CUDA_MEMCPY2D *pCopy, CUstream hStream)
 * cuMemcpy3D_v2(const CUDA_MEMCPY3D *pCopy)
 * cuMemcpy3DAsync_v2(const CUDA_MEMCPY3D *pCopy, CUstream hStream)
 *
 * 모두 PARM1 이 구조체 포인터다. async 변형의 stream 인자는 본 모듈에서 사용하지 않아 무시한다.
 */
SEC("uprobe/cuMemcpy2D_v2")
int BPF_UPROBE(handle_cu_memcpy_2d)
{
    emit_memcpy2d((__u64)PT_REGS_PARM1(ctx));
    return 0;
}

SEC("uprobe/cuMemcpy2DAsync_v2")
int BPF_UPROBE(handle_cu_memcpy_2d_async)
{
    emit_memcpy2d((__u64)PT_REGS_PARM1(ctx));
    return 0;
}

SEC("uprobe/cuMemcpy3D_v2")
int BPF_UPROBE(handle_cu_memcpy_3d)
{
    emit_memcpy3d((__u64)PT_REGS_PARM1(ctx));
    return 0;
}

SEC("uprobe/cuMemcpy3DAsync_v2")
int BPF_UPROBE(handle_cu_memcpy_3d_async)
{
    emit_memcpy3d((__u64)PT_REGS_PARM1(ctx));
    return 0;
}

/*
 * cuMemcpy(CUdeviceptr dst, CUdeviceptr src, size_t ByteCount)
 *
 * UVA (Unified Virtual Addressing) 환경에서 dst / src 모두 CUdeviceptr 로 표현되어 BPF 단에서는
 * host / device 구분이 불가능하다. cuPointerGetAttribute(CU_POINTER_ATTRIBUTE_MEMORY_TYPE) 호출이
 * 필요하지만 BPF 안에서 호출할 수 없으므로 본 심볼의 모든 이벤트는 UNKNOWN_DIR 로 emit 한다.
 * 운영자는 metric 으로 비중을 보고 추가 분석을 결정한다.
 */
SEC("uprobe/cuMemcpy")
int BPF_UPROBE(handle_cu_memcpy)
{
    __u64 bytes = (__u64)PT_REGS_PARM3(ctx);
    emit_event(CUDA_EVENT_UNKNOWN_DIR, bytes);
    return 0;
}
