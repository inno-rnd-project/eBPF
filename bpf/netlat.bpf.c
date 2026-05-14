#include "vmlinux.h"
#include "common.h"

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_endian.h>

char LICENSE[] SEC("license") = "GPL";

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 16384);
    __type(key, __u32);                      /* tid */
    __type(value, struct netobs_start_info);
} starts SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 24);            /* 16 MiB */
} events SEC(".maps");

/* key=0, value=target dst IPv4 in network byte order, 0이면 비활성화 */
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u32);
} target_daddr SEC(".maps");

/* pod_bytes 는 Pod 단위 RX/TX bytes / packets 누적 맵이다. LRU_PERCPU_HASH 라 hot path 의 CPU 간
 * contention 이 없고, max_entries 도달 시 오래된 entry 가 자연 evict 되어 종료 Pod 의 stale 시리즈
 * cleanup 부담을 BPF 측에서 자동 해소한다. max_entries 16384 는 (활성 Pod 4096) x (direction 2) x
 * (layer 2) = 16384 슬롯 예산으로 산정했다. Go 측 collector 가 scrape 시점에 본 맵을 iterate 해
 * 누적치를 그대로 Prometheus counter 로 emit 한다. */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_PERCPU_HASH);
    __uint(max_entries, 16384);
    __type(key, struct netobs_pod_bytes_key);
    __type(value, struct netobs_pod_bytes_value);
} pod_bytes SEC(".maps");

static __always_inline int match_target(__u32 daddr_net)
{
    __u32 key = 0;
    __u32 *target = bpf_map_lookup_elem(&target_daddr, &key);
    if (!target || *target == 0)
        return 1;
    return daddr_net == *target;
}

static __always_inline __u64 get_socket_cookie(struct sock *sk)
{
    if (!sk)
        return 0;

    /* atomic64_t → .counter로 접근해야 __u64 반환 */
    return BPF_CORE_READ(sk, __sk_common.skc_cookie.counter);
}

static __always_inline __u32 get_dev_ifindex(struct net_device *dev)
{
    if (!dev)
        return 0;
    return BPF_CORE_READ(dev, ifindex);
}

static __always_inline __u32 get_skb_ifindex(struct sk_buff *skb)
{
    if (!skb)
        return 0;
    return get_dev_ifindex(BPF_CORE_READ(skb, dev));
}

static __always_inline __u32 get_skb_iif(struct sk_buff *skb)
{
    if (!skb)
        return 0;
    return BPF_CORE_READ(skb, skb_iif);
}

static __always_inline void fill_conn_from_sock(struct sock *sk, struct netobs_start_info *s)
{
    s->saddr = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
    s->daddr = BPF_CORE_READ(sk, __sk_common.skc_daddr);
    s->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
    s->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
    s->socket_cookie = get_socket_cookie(sk);

    s->ifindex = BPF_CORE_READ(sk, __sk_common.skc_bound_dev_if);
    s->skb_iif = 0;
}

static __always_inline void fill_dev_from_skb(struct sk_buff *skb, struct netobs_start_info *s)
{
    __u32 ifindex;
    __u32 skb_iif;

    if (!skb || !s)
        return;

    ifindex = get_skb_ifindex(skb);
    skb_iif = get_skb_iif(skb);

    if (ifindex)
        s->ifindex = ifindex;
    if (skb_iif)
        s->skb_iif = skb_iif;
}

/* inc_pod_bytes는 (cgroup_id, direction, layer) 삼중 키로 bytes/packets를 1회 누적한다.
 * BPF_MAP_TYPE_LRU_PERCPU_HASH는 lookup이 per-CPU 슬롯을 반환하므로 atomic 연산 없이 단순 덧셈으로
 * race가 없다. cgroup_id가 0이면 host 작업 또는 cgroup v2 미지원 컨텍스트라 무시한다. */
static __always_inline void inc_pod_bytes(__u64 cgroup_id, __u8 direction, __u8 layer, __u64 bytes_delta)
{
    struct netobs_pod_bytes_key key;
    struct netobs_pod_bytes_value *val;
    struct netobs_pod_bytes_value init;

    if (!cgroup_id)
        return;

    __builtin_memset(&key, 0, sizeof(key));
    key.cgroup_id = cgroup_id;
    key.direction = direction;
    key.layer     = layer;

    val = bpf_map_lookup_elem(&pod_bytes, &key);
    if (val) {
        val->bytes   += bytes_delta;
        val->packets += 1;
        return;
    }

    __builtin_memset(&init, 0, sizeof(init));
    init.bytes   = bytes_delta;
    init.packets = 1;
    bpf_map_update_elem(&pod_bytes, &key, &init, BPF_ANY);
}

static __always_inline void emit_event(const struct netobs_start_info *s,
                                       __u8 stage,
                                       __u32 reason,
                                       __u32 ret,
                                       __u32 latency_us)
{
    struct netobs_event *e;

    e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
    if (!e)
        return;

    e->ts_ns         = bpf_ktime_get_ns();
    e->cgroup_id     = s->cgroup_id;
    e->socket_cookie = s->socket_cookie;
    e->saddr         = s->saddr;
    e->daddr         = s->daddr;
    e->pid           = s->pid;
    e->tid           = s->tid;
    e->ret           = ret;
    e->latency_us    = latency_us;
    e->reason        = reason;
    e->ifindex       = s->ifindex;
    e->skb_iif       = s->skb_iif;
    e->sport         = s->sport;
    e->dport         = s->dport;

    __builtin_memcpy(e->comm, s->comm, sizeof(e->comm));

    e->stage         = stage;
    e->pad[0]        = 0;
    e->pad[1]        = 0;
    e->pad[2]        = 0;

    bpf_ringbuf_submit(e, 0);
}

SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(handle_tcp_sendmsg, struct sock *sk, struct msghdr *msg, size_t size)
{
    struct netobs_start_info s = {};
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    s.ts_ns     = bpf_ktime_get_ns();
    s.cgroup_id = bpf_get_current_cgroup_id();
    s.pid       = pid_tgid >> 32;
    s.tid       = (__u32)pid_tgid;

    fill_conn_from_sock(sk, &s);
    if (!match_target(s.daddr))
        return 0;

    /* L4 syscall layer egress 바이트 누적. tcp_sendmsg의 size 인자는 사용자가 send에 넘긴 페이로드
     * 크기로 헤더가 빠져 있어 cAdvisor의 NIC layer bytes와는 헤더 분량만큼 차이가 난다. L4 layer는
     * 워크로드 자체의 데이터 송신량 정량화에 쓰인다. */
    inc_pod_bytes(s.cgroup_id, NETOBS_DIR_EGRESS, NETOBS_LAYER_L4, (__u64)size);

    bpf_get_current_comm(&s.comm, sizeof(s.comm));
    bpf_map_update_elem(&starts, &s.tid, &s, BPF_ANY);
    return 0;
}

SEC("kretprobe/tcp_sendmsg")
int BPF_KRETPROBE(handle_tcp_sendmsg_ret, int ret)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct netobs_start_info *s;
    __u64 now;
    __u32 latency_us;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s)
        return 0;

    now = bpf_ktime_get_ns();
    latency_us = (__u32)((now - s->ts_ns) / 1000);

    emit_event(s, NETOBS_STAGE_SENDMSG_RET, 0, ret, latency_us);
    s->ret_seen = 1;

    if (s->seen_veth && s->seen_devq)
        bpf_map_delete_elem(&starts, &tid);

    return 0;
}

SEC("kprobe/veth_xmit")
int BPF_KPROBE(handle_veth_xmit, struct sk_buff *skb)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct netobs_start_info *s;
    __u64 now;
    __u32 latency_us;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s || s->seen_veth)
        return 0;

    fill_dev_from_skb(skb, s);

    now = bpf_ktime_get_ns();
    latency_us = (__u32)((now - s->ts_ns) / 1000);

    emit_event(s, NETOBS_STAGE_TO_VETH, 0, 0, latency_us);
    s->seen_veth = 1;

    if (s->ret_seen && s->seen_devq)
        bpf_map_delete_elem(&starts, &tid);

    return 0;
}

SEC("kprobe/__dev_queue_xmit")
int BPF_KPROBE(handle_dev_queue_xmit, struct sk_buff *skb)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct netobs_start_info *s;
    __u64 now;
    __u32 latency_us;
    __u64 skb_len;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s)
        return 0;

    /* NIC layer egress 바이트는 sendmsg 한 번이 만들어내는 모든 segment에 대해 누적해야 cAdvisor의
     * container_network_*_bytes_total과 정합 가능하다. seen_devq는 NETOBS_STAGE_TO_DEVQ 이벤트
     * 중복 방출만 가드하는 플래그이므로 bytes 누적은 본 플래그와 분리해 매 진입마다 수행한다. */
    fill_dev_from_skb(skb, s);
    skb_len = (__u64)BPF_CORE_READ(skb, len);
    inc_pod_bytes(s->cgroup_id, NETOBS_DIR_EGRESS, NETOBS_LAYER_NIC, skb_len);

    if (s->seen_devq)
        return 0;

    now = bpf_ktime_get_ns();
    latency_us = (__u32)((now - s->ts_ns) / 1000);

    emit_event(s, NETOBS_STAGE_TO_DEVQ, 0, 0, latency_us);
    s->seen_devq = 1;

    if (s->ret_seen && s->seen_veth)
        bpf_map_delete_elem(&starts, &tid);

    return 0;
}

SEC("kprobe/tcp_retransmit_skb")
int BPF_KPROBE(handle_tcp_retransmit_skb, struct sock *sk, struct sk_buff *skb, int segs)
{
    struct netobs_start_info s = {};
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    s.ts_ns     = bpf_ktime_get_ns();
    s.cgroup_id = bpf_get_current_cgroup_id();
    s.pid       = pid_tgid >> 32;
    s.tid       = (__u32)pid_tgid;

    fill_conn_from_sock(sk, &s);
    fill_dev_from_skb(skb, &s);

    if (!match_target(s.daddr))
        return 0;

    bpf_get_current_comm(&s.comm, sizeof(s.comm));
    emit_event(&s, NETOBS_STAGE_RETRANS, 0, 0, 0);
    return 0;
}

/* tcp_cleanup_rbuf는 사용자 공간이 TCP 수신 버퍼에서 실제로 읽어간 바이트를 보고하는 시점에
 * 호출된다. recvmsg 경로에서 process context로 실행되어 bpf_get_current_cgroup_id()가 수신 Pod의
 * cgroup_id를 정확히 반환하므로 ingress L4 카운터의 표준 hook으로 적합하다. copied 인자는
 * 음수일 때 에러 신호이므로 양수일 때만 누적한다. NIC layer ingress는 softirq context에서
 * cgroup 해상이 복잡해 본 PR 범위 밖이며 follow-up 이슈에서 다룬다. */
SEC("kprobe/tcp_cleanup_rbuf")
int BPF_KPROBE(handle_tcp_cleanup_rbuf, struct sock *sk, int copied)
{
    __u64 cgroup_id;
    __u32 daddr;

    if (copied <= 0)
        return 0;

    cgroup_id = bpf_get_current_cgroup_id();
    if (!cgroup_id)
        return 0;

    daddr = BPF_CORE_READ(sk, __sk_common.skc_daddr);
    if (!match_target(daddr))
        return 0;

    inc_pod_bytes(cgroup_id, NETOBS_DIR_INGRESS, NETOBS_LAYER_L4, (__u64)copied);
    return 0;
}

SEC("kprobe/kfree_skb_reason")
int BPF_KPROBE(handle_kfree_skb_reason, struct sk_buff *skb, int reason)
{
    struct sock *sk;
    struct netobs_start_info s = {};
    __u64 pid_tgid = bpf_get_current_pid_tgid();

    sk = BPF_CORE_READ(skb, sk);
    if (!sk)
        return 0;

    s.ts_ns     = bpf_ktime_get_ns();
    s.cgroup_id = bpf_get_current_cgroup_id();
    s.pid       = pid_tgid >> 32;
    s.tid       = (__u32)pid_tgid;

    fill_conn_from_sock(sk, &s);
    fill_dev_from_skb(skb, &s);

    if (!match_target(s.daddr))
        return 0;

    bpf_get_current_comm(&s.comm, sizeof(s.comm));
    emit_event(&s, NETOBS_STAGE_DROP, reason, 0, 0);
    return 0;
}
