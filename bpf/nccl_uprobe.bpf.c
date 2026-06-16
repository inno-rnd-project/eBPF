// gpuobs NCCL collective uprobe BPF 프로그램 (#134).
//
// libnccl.so.2 의 collective communication 심볼에 uprobe (entry) 와 uretprobe (exit) 를 걸어
// collective operation 의 wall-clock 분포를 ringbuf 로 emit 한다. multi-rank distributed training
// 워크로드 (PyTorch DDP, Megatron 등) 에서 rank 간 sync 대기로 GPU 가 유휴 상태가 되는 케이스를
// nccl_collective_stall dominant cause 의 base score 입력으로 활용한다.
//
// 심볼 선택 근거:
//   * ncclAllReduce  - DDP gradient all-reduce. distributed training 의 가장 빈번한 collective.
//   * ncclBroadcast  - 초기 weight broadcast 와 parameter 동기화.
//   * ncclReduceScatter / ncclAllGather - ZeRO / FSDP 의 sharded gradient 와 parameter 경로.
//
// 측정 한계: NCCL 의 ncclComm 핸들은 opaque internal struct 라 rank count 를 BTF 로 추출 불가.
// 따라서 rank_count 는 0 (미측정) 으로 두고 collective 의 wall-clock (entry-exit latency) 만
// 측정한다. userspace 가 본 latency 를 collective duration histogram 으로 변환하고 recording rule
// 이 그 rate 를 정규화해 nccl_collective_stall score 를 산출한다.
//
// userspace 측 PID→GPU 귀속은 gpuobs 의 기존 cuda devicemap 캐시와 동일 원리로 수행하므로 본 BPF
// 프로그램은 GPU 식별자를 모른다. 이벤트는 prog→user 의 thin pipe 역할만 한다.

#include "vmlinux.h"

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

// nccl_op 는 collective 종류 enum 이다. userspace 의 Go 측 operationName 매핑과 1:1 정합한다.
enum nccl_op {
    NCCL_OP_ALLREDUCE     = 1,
    NCCL_OP_BROADCAST     = 2,
    NCCL_OP_REDUCESCATTER = 3,
    NCCL_OP_ALLGATHER     = 4,
};

// nccl_event 는 collective operation 단일 sample 표현이다. userspace 의 Go 측 구조체와 layout 이
// 정확히 일치해야 한다 (binary.NativeEndian Read). 32 bytes 고정 — 8(ts_ns) + 8(duration_ns) +
// 4(pid) + 4(tid) + 4(rank_count) + 1(op) + 3(pad).
struct nccl_event {
    __u64 ts_ns;
    __u64 duration_ns;
    __u32 pid;
    __u32 tid;
    __u32 rank_count;
    __u8  op;
    __u8  pad[3];
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 22);    /* 4 MiB */
} nccl_events SEC(".maps");

// nccl_dropped 는 ringbuf reserve 가 NULL 을 돌려준 (가득 참) 케이스를 percpu 로 누적한다. cuda
// uprobe 의 cuda_dropped 와 동일 패턴이다. userspace 가 주기적으로 read + sum 해 self-health
// counter 로 노출한다.
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u64);
} nccl_dropped SEC(".maps");

// nccl_start_key 는 collective entry ts 를 stash 하는 키다. tid 와 op 분리로 한 thread 의 nested
// collective 호출이 서로 덮어쓰지 않게 한다. cuda uprobe 의 sync_start_key 와 동일 패턴이다.
struct nccl_start_key {
    __u32 tid;
    __u8  op;
    __u8  pad[3];
};

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, struct nccl_start_key);
    __type(value, __u64);
} nccl_starts SEC(".maps");

// inc_dropped 는 nccl_dropped percpu 카운터를 증가시킨다.
static __always_inline void inc_dropped(void)
{
    __u32 key = 0;
    __u64 *cnt = bpf_map_lookup_elem(&nccl_dropped, &key);
    if (cnt)
        (*cnt)++;
}

// collective_entry 는 collective entry uprobe 가 ts_ns 를 nccl_starts 에 stash 한다.
static __always_inline void collective_entry(__u8 op)
{
    struct nccl_start_key key = {
        .tid = (__u32)bpf_get_current_pid_tgid(),
        .op  = op,
    };
    __u64 now = bpf_ktime_get_ns();
    bpf_map_update_elem(&nccl_starts, &key, &now, BPF_ANY);
}

// collective_exit 는 collective uretprobe 가 entry ts 를 lookup 해 wall-clock 을 산정하고 ringbuf
// event 를 emit 한다. entry stash 가 없으면 (attach 이전 호출 또는 stale) emit 을 skip 해 0 sample
// 잡음이 histogram 에 끼지 않게 한다. rank_count 는 ncclComm opaque 제약으로 0 으로 둔다.
static __always_inline void collective_exit(__u8 op)
{
    struct nccl_start_key key = {
        .tid = (__u32)bpf_get_current_pid_tgid(),
        .op  = op,
    };
    __u64 *start = bpf_map_lookup_elem(&nccl_starts, &key);
    if (!start)
        return;
    __u64 duration = bpf_ktime_get_ns() - *start;
    bpf_map_delete_elem(&nccl_starts, &key);

    struct nccl_event *e = bpf_ringbuf_reserve(&nccl_events, sizeof(*e), 0);
    if (!e) {
        inc_dropped();
        return;
    }
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    e->ts_ns       = bpf_ktime_get_ns();
    e->duration_ns = duration;
    e->pid         = pid_tgid >> 32;
    e->tid         = (__u32)pid_tgid;
    e->rank_count  = 0;
    e->op          = op;
    e->pad[0]      = 0;
    e->pad[1]      = 0;
    e->pad[2]      = 0;
    bpf_ringbuf_submit(e, 0);
}

SEC("uprobe.multi/ncclAllReduce")
int BPF_UPROBE(handle_nccl_allreduce_entry)
{
    collective_entry(NCCL_OP_ALLREDUCE);
    return 0;
}

SEC("uretprobe.multi/ncclAllReduce")
int BPF_URETPROBE(handle_nccl_allreduce_exit)
{
    collective_exit(NCCL_OP_ALLREDUCE);
    return 0;
}

SEC("uprobe.multi/ncclBroadcast")
int BPF_UPROBE(handle_nccl_broadcast_entry)
{
    collective_entry(NCCL_OP_BROADCAST);
    return 0;
}

SEC("uretprobe.multi/ncclBroadcast")
int BPF_URETPROBE(handle_nccl_broadcast_exit)
{
    collective_exit(NCCL_OP_BROADCAST);
    return 0;
}

SEC("uprobe.multi/ncclReduceScatter")
int BPF_UPROBE(handle_nccl_reducescatter_entry)
{
    collective_entry(NCCL_OP_REDUCESCATTER);
    return 0;
}

SEC("uretprobe.multi/ncclReduceScatter")
int BPF_URETPROBE(handle_nccl_reducescatter_exit)
{
    collective_exit(NCCL_OP_REDUCESCATTER);
    return 0;
}

SEC("uprobe.multi/ncclAllGather")
int BPF_UPROBE(handle_nccl_allgather_entry)
{
    collective_entry(NCCL_OP_ALLGATHER);
    return 0;
}

SEC("uretprobe.multi/ncclAllGather")
int BPF_URETPROBE(handle_nccl_allgather_exit)
{
    collective_exit(NCCL_OP_ALLGATHER);
    return 0;
}
