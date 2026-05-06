// gpuobs CUDA uprobe BPF 프로그램.
//
// libcuda.so.1 의 9개 심볼에 uprobe 를 걸어 (pid, kind, bytes) 이벤트를 ringbuf 로 emit 한다.
// 심볼 선택 근거:
//   * cuLaunchKernel / cuLaunchKernelEx / cuLaunchCooperativeKernel
//     - CUDA Driver API 의 모든 커널 런치 경로. PyTorch / TF 가 둘 다 통과한다.
//   * cuMemcpyHtoD_v2 / cuMemcpyHtoDAsync_v2 / cuMemcpyDtoH_v2 / cuMemcpyDtoHAsync_v2
//     - 방향이 명확한 host↔device 메모리 전송 경로. ByteCount(rdx) 를 그대로 발행한다.
//   * cuMemcpyDtoD_v2 / cuMemcpyDtoDAsync_v2
//     - 방향이 항상 device→device 인 경로. ByteCount(rdx) 를 그대로 발행한다 (#31 1단계).
//
// 방향이 인자 ptr type 또는 구조체 필드에서 결정되는 cuMemcpy / cuMemcpy2D* / cuMemcpy3D* 와
// CUDA Runtime API (cudaMemcpy*, cudaLaunchKernel) 는 본 PR 의 후속 commit 들에서 단계적으로 추가된다.
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
};

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
