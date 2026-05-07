// gpuobs CUDA uprobe BPF 프로그램.
//
// libcuda.so.1 의 14개 심볼과 libcudart.so 의 3개 심볼에 uprobe 를 걸어 (pid, kind, bytes) 이벤트를 ringbuf 로 emit 한다.
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
//   * cudaLaunchKernel / cudaMemcpy / cudaMemcpyAsync (libcudart.so)
//     - CUDA Runtime API. cudaMemcpy* 의 PARM4 가 cudaMemcpyKind enum 이라 그것을 그대로 분기에
//       사용해 h2d / d2h / dtod / unknown_dir 로 분류한다. 운영 환경에 따라 컨테이너가 자체 libcudart
//       를 번들링하는 경우 host attach 가 fire 되지 않을 수 있으며, 이는 README 한계 note 에 명시한다.
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
// 32 bytes 고정 — 8(ts) + 8(bytes) + 4(pid) + 4(tid) + 1(kind) + 3(pad) + 4(device_ord).
//
// device_ord 는 emit 시점에 cuda_tid_device map 에서 lookup 한 컨테이너 CUDA driver 의 ordinal
// 이며, 매핑이 없으면 CUDA_DEVICE_ORD_UNKNOWN (= 0xFFFFFFFF) 로 발행되어 userspace 가 기존
// PID-level devmap.lookup 폴백을 적용한다 (#33).
struct cuda_event {
    __u64 ts_ns;
    __u64 bytes;     /* memcpy 시 ByteCount; kernel launch 시 0 */
    __u32 pid;
    __u32 tid;
    __u8  kind;
    __u8  pad[3];
    __u32 device_ord;
};

#define CUDA_DEVICE_ORD_UNKNOWN 0xFFFFFFFFU

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

/* cuda_tid_device 는 호스트 TID 를 컨테이너 CUDA driver 의 device ordinal 로 매핑한다.
 * cudaSetDevice / cuCtxSetCurrent uprobe 가 본 map 을 갱신하고, kernel launch / memcpy uprobe
 * 가 emit 직전에 lookup 해 이벤트에 device ordinal 을 첨부한다.
 *
 * LRU_HASH 를 사용해 종료된 thread 의 stale 엔트리가 자동 evict 되도록 한다 (별도 cleanup
 * 경로 불요). max_entries 는 동시 활성 CUDA thread 수의 worst case 를 16384 로 잡는다 —
 * PyTorch 류의 일반 워크로드는 코어 수 + worker thread 수 합계가 수백 단위라 충분.
 */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, __u32);
    __type(value, __u32);
} cuda_tid_device SEC(".maps");

/* cuctx_to_device 는 CUDA Driver API 의 CUcontext 포인터를 device ordinal 로 매핑한다.
 * cuCtxCreate_v2 의 uprobe + uretprobe 페어가 (출력 ctx 포인터, PARM3 device) 를 본 map 에
 * 적재하고, cuCtxSetCurrent uprobe 가 인자로 받은 ctx 를 lookup 해 cuda_tid_device 의 TID
 * 매핑을 갱신한다. cuCtxDestroy 같은 정리 hook 은 두지 않으며, LRU 정책으로 stale entry 가
 * 자동 evict 된다.
 */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 4096);
    __type(key, __u64);
    __type(value, __u32);
} cuctx_to_device SEC(".maps");

/* cuctx_create_args 는 cuCtxCreate_v2 의 uprobe (entry) 가 capture 한 출력 포인터 + device
 * ordinal 을 같은 호출의 uretprobe (exit) 로 전달하는 임시 map 이다. uretprobe 시점에는
 * x86_64 ABI 규약상 entry 의 PARM 레지스터가 전부 clobber 되어 있어 entry 에서 상태를 보관해
 * 두지 않으면 (pctx 출력 위치, dev) 를 알 수 없다. exit 가 lookup 후 즉시 delete 한다.
 */
struct ctx_create_args {
    __u64 pctx_addr;
    __u32 dev;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u32);
    __type(value, struct ctx_create_args);
} cuctx_create_args SEC(".maps");

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
    __u32 tid = (__u32)pid_tgid;
    e->ts_ns = bpf_ktime_get_ns();
    e->bytes = bytes;
    e->pid   = pid_tgid >> 32;
    e->tid   = tid;
    e->kind  = kind;
    e->pad[0] = 0;
    e->pad[1] = 0;
    e->pad[2] = 0;

    /* cuda_tid_device 에 매핑이 없으면 (cudaSetDevice / cuCtxSetCurrent 가 한 번도 호출되지 않은
     * 단일 GPU 워크로드, 또는 cuCtxCreate_v3 / cuDevicePrimaryCtxRetain 등 본 PR scope 외 경로로
     * context 가 만들어진 경우) UNKNOWN sentinel 로 발행해 userspace 가 PID-level devmap.lookup
     * 폴백을 적용하게 한다.
     */
    __u32 *dev = bpf_map_lookup_elem(&cuda_tid_device, &tid);
    e->device_ord = dev ? *dev : CUDA_DEVICE_ORD_UNKNOWN;

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
 * 총 byte 크기와 함께 emit 한다. pCopy 가 NULL 이거나 어느 하나라도 read 가 실패하면 emit 하지 않아
 * userspace 측에 0-byte unknown_dir 시리즈가 헛생성되지 않게 한다. width / height (3D 는 depth 포함) 중
 * 하나라도 0 인 경우도 동일 이유로 skip — 카운터에 0 을 더하면 값은 변하지 않지만 라벨 키는
 * seenCudaKeys 에 박혀 cleanup 사이클이 영구히 도는 데드 시리즈가 된다.
 */
static __always_inline void emit_memcpy2d(__u64 pCopy)
{
    if (!pCopy)
        return;
    __u32 src_mt = 0, dst_mt = 0;
    if (bpf_probe_read_user(&src_mt, sizeof(src_mt), (const void *)(pCopy + MEMCPY2D_SRC_MEMTYPE_OFF)) < 0)
        return;
    if (bpf_probe_read_user(&dst_mt, sizeof(dst_mt), (const void *)(pCopy + MEMCPY2D_DST_MEMTYPE_OFF)) < 0)
        return;
    __u64 width = 0, height = 0;
    if (bpf_probe_read_user(&width, sizeof(width), (const void *)(pCopy + MEMCPY2D_WIDTH_BYTES_OFF)) < 0)
        return;
    if (bpf_probe_read_user(&height, sizeof(height), (const void *)(pCopy + MEMCPY2D_HEIGHT_OFF)) < 0)
        return;
    if (width == 0 || height == 0)
        return;
    emit_event(dir_from_memtype(src_mt, dst_mt), width * height);
}

static __always_inline void emit_memcpy3d(__u64 pCopy)
{
    if (!pCopy)
        return;
    __u32 src_mt = 0, dst_mt = 0;
    if (bpf_probe_read_user(&src_mt, sizeof(src_mt), (const void *)(pCopy + MEMCPY3D_SRC_MEMTYPE_OFF)) < 0)
        return;
    if (bpf_probe_read_user(&dst_mt, sizeof(dst_mt), (const void *)(pCopy + MEMCPY3D_DST_MEMTYPE_OFF)) < 0)
        return;
    __u64 width = 0, height = 0, depth = 0;
    if (bpf_probe_read_user(&width, sizeof(width), (const void *)(pCopy + MEMCPY3D_WIDTH_BYTES_OFF)) < 0)
        return;
    if (bpf_probe_read_user(&height, sizeof(height), (const void *)(pCopy + MEMCPY3D_HEIGHT_OFF)) < 0)
        return;
    if (bpf_probe_read_user(&depth, sizeof(depth), (const void *)(pCopy + MEMCPY3D_DEPTH_OFF)) < 0)
        return;
    if (width == 0 || height == 0 || depth == 0)
        return;
    emit_event(dir_from_memtype(src_mt, dst_mt), width * height * depth);
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

/* CUDA Runtime API (libcudart.so) 의 cudaMemcpyKind enum 값. cuda_runtime_api.h 와 1:1.
 * cudaMemcpyDefault (=4) 는 UVA 라 driver 가 결정해 BPF 가 모르므로 UNKNOWN_DIR 로 분류한다.
 */
#define CUDA_MEMCPY_HOST_TO_HOST     0
#define CUDA_MEMCPY_HOST_TO_DEVICE   1
#define CUDA_MEMCPY_DEVICE_TO_HOST   2
#define CUDA_MEMCPY_DEVICE_TO_DEVICE 3
#define CUDA_MEMCPY_DEFAULT          4

static __always_inline __u8 cudart_dir_from_kind(__u32 kind)
{
    if (kind == CUDA_MEMCPY_HOST_TO_DEVICE)
        return CUDA_EVENT_H2D;
    if (kind == CUDA_MEMCPY_DEVICE_TO_HOST)
        return CUDA_EVENT_D2H;
    if (kind == CUDA_MEMCPY_DEVICE_TO_DEVICE)
        return CUDA_EVENT_DTOD;
    /* HOST_TO_HOST / DEFAULT / 기타 */
    return CUDA_EVENT_UNKNOWN_DIR;
}

/*
 * cudaError_t cudaLaunchKernel(const void *func, dim3 gridDim, dim3 blockDim, void **args,
 *                              size_t sharedMem, cudaStream_t stream)
 *
 * Runtime API 의 커널 런치. driver API 의 cuLaunchKernel 과 동일하게 카운터 이벤트만 emit 한다.
 */
SEC("uprobe/cudaLaunchKernel")
int BPF_UPROBE(handle_cuda_launch_kernel)
{
    emit_event(CUDA_EVENT_KERNEL_LAUNCH, 0);
    return 0;
}

/*
 * cudaError_t cudaMemcpy(void *dst, const void *src, size_t count, enum cudaMemcpyKind kind)
 * cudaError_t cudaMemcpyAsync(void *dst, const void *src, size_t count,
 *                             enum cudaMemcpyKind kind, cudaStream_t stream)
 *
 * 두 심볼 모두 PARM3 = count, PARM4 = cudaMemcpyKind enum (4 bytes signed int 이지만 64-bit 레지스터
 * 의 하위 32 비트만 의미). cudart_dir_from_kind 가 enum 을 cuda_event_kind 로 변환한다.
 */
SEC("uprobe/cudaMemcpy")
int BPF_UPROBE(handle_cuda_memcpy)
{
    __u64 count = (__u64)PT_REGS_PARM3(ctx);
    __u32 kind = (__u32)PT_REGS_PARM4(ctx);
    emit_event(cudart_dir_from_kind(kind), count);
    return 0;
}

SEC("uprobe/cudaMemcpyAsync")
int BPF_UPROBE(handle_cuda_memcpy_async)
{
    __u64 count = (__u64)PT_REGS_PARM3(ctx);
    __u32 kind = (__u32)PT_REGS_PARM4(ctx);
    emit_event(cudart_dir_from_kind(kind), count);
    return 0;
}

/*
 * cudaError_t cudaSetDevice(int device)
 *
 * thread-local current device 를 설정한다. 본 thread 의 후속 cudaLaunchKernel / cudaMemcpy*
 * 호출은 모두 본 device 에서 실행되므로, TID → device ordinal 매핑을 cuda_tid_device map 에
 * 기록해 dispatch 시점의 GPU attribution 을 정확히 분리한다 (#33).
 *
 * device 값은 4 bytes signed int 이지만 64-bit 레지스터의 하위 32 비트에서 읽어 unsigned 으로
 * 다룬다. 음수 값 (예: cudaInvalidDeviceId == -1) 은 매우 큰 unsigned 으로 해석되어 NVML
 * device count 범위 밖이라 ordinal-to-UUID 매핑 단계에서 자연스럽게 빈 문자열로 폴백된다.
 */
SEC("uprobe/cudaSetDevice")
int BPF_UPROBE(handle_cuda_set_device)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    __u32 device = (__u32)PT_REGS_PARM1(ctx);
    bpf_map_update_elem(&cuda_tid_device, &tid, &device, BPF_ANY);
    return 0;
}

/*
 * CUresult cuCtxCreate_v2(CUcontext *pctx, unsigned int flags, CUdevice dev)
 *
 * uprobe (entry) 는 PARM1 (pctx 출력 포인터 주소) 와 PARM3 (CUdevice ordinal) 를 cuctx_create_args
 * 임시 map 에 보관한다. uretprobe (exit) 는 *pctx_addr 의 resolved CUcontext 값을 bpf_probe_read_user
 * 로 읽어 cuctx_to_device 매핑을 완성한다. cuCtxCreate_v3 등 다른 v 변형은 본 PR 의 scope 외로
 * 미루며 (인자 위치가 다름), Driver API 사용자가 v2 를 거의 항상 쓰는 일반 가정에 의존한다.
 */
SEC("uprobe/cuCtxCreate_v2")
int BPF_UPROBE(handle_cu_ctx_create_v2_entry)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct ctx_create_args args = {
        .pctx_addr = (__u64)PT_REGS_PARM1(ctx),
        .dev = (__u32)PT_REGS_PARM3(ctx),
    };
    bpf_map_update_elem(&cuctx_create_args, &tid, &args, BPF_ANY);
    return 0;
}

SEC("uretprobe/cuCtxCreate_v2")
int BPF_URETPROBE(handle_cu_ctx_create_v2_exit)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct ctx_create_args *args = bpf_map_lookup_elem(&cuctx_create_args, &tid);
    if (!args)
        return 0;
    __u64 cuctx_val = 0;
    if (bpf_probe_read_user(&cuctx_val, sizeof(cuctx_val), (const void *)args->pctx_addr) == 0 && cuctx_val != 0) {
        __u32 dev = args->dev;
        bpf_map_update_elem(&cuctx_to_device, &cuctx_val, &dev, BPF_ANY);
    }
    bpf_map_delete_elem(&cuctx_create_args, &tid);
    return 0;
}

/*
 * CUresult cuCtxSetCurrent(CUcontext ctx)
 *
 * 본 thread 의 current CUcontext 를 변경한다. PARM1 의 ctx 를 cuctx_to_device 에서 lookup 해
 * cuda_tid_device 의 TID → device 매핑을 갱신한다. cuCtxCreate_v2 hook 이 없는 (예: cuCtxCreate_v3
 * 또는 cuDevicePrimaryCtxRetain 으로 만들어진) 컨텍스트에 대해서는 lookup miss 라 매핑 갱신이
 * 일어나지 않고 dispatch 시점에는 cuda_tid_device 의 직전 매핑이 유지된다. 이 케이스는 본 PR 의
 * scope 외로 한계 note 에 명시한다.
 */
SEC("uprobe/cuCtxSetCurrent")
int BPF_UPROBE(handle_cu_ctx_set_current)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    __u64 cuctx_val = (__u64)PT_REGS_PARM1(ctx);
    if (cuctx_val == 0)
        return 0;
    __u32 *dev = bpf_map_lookup_elem(&cuctx_to_device, &cuctx_val);
    if (!dev)
        return 0;
    bpf_map_update_elem(&cuda_tid_device, &tid, dev, BPF_ANY);
    return 0;
}
