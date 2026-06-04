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

/* events_dropped 는 events ringbuf 의 bpf_ringbuf_reserve 가 NULL 을 돌려준 케이스 (ringbuf 가
 * 가득 차서 record 를 못 잡은 상황) 를 percpu 카운터로 누적한다. cilium/ebpf 의 ringbuf API 가
 * lost sample 카운터를 노출하지 않아 cuda 측 cuda_dropped 와 동일한 패턴으로 BPF 측에서 직접
 * 카운트한다. userspace 가 주기적으로 read + sum 해 netobs_bpf_ringbuf_drops_total 카운터에
 * baseline-then-delta 로 add 한다. percpu 라 producer 측 atomic 비용이 0 이다.
 */
struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u64);
} events_dropped SEC(".maps");

/* key=0, value=target dst IPv4 in network byte order, 0이면 비활성화 */
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, __u32);
} target_daddr SEC(".maps");

/* #83 drop event 발생 시점의 kernel stack trace 를 보관하는 stack id 맵이다. handle_kfree_skb_reason
 * 의 bpf_get_stackid 가 본 맵에 stack frame 배열을 적재하고 stack id 만 ringbuf event 에 carry 하는
 * 패턴이다. PERF_MAX_STACK_DEPTH 는 linux/perf_event.h 의 127 로 vmlinux 의존성 없이 hardcode 한다.
 * max_entries 10240 은 unique stack 의 일반적인 cap 이며 userspace symbol resolver 의 LRU cache cap
 * 1024 와 함께 본 PR 의 cardinality 가드 첫 단계로 동작한다. */
#define NETOBS_PERF_MAX_STACK_DEPTH 127
struct {
    __uint(type, BPF_MAP_TYPE_STACK_TRACE);
    __uint(max_entries, 10240);
    __type(key, __u32);
    __type(value, __u64[NETOBS_PERF_MAX_STACK_DEPTH]);
} drop_stacks SEC(".maps");

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

/* #85 Pod 간 정상 flow 의 5-tuple RX/TX 누적 맵이다. pod_bytes 의 LRU_PERCPU_HASH 와 분리해 per-CPU
 * × 1024 의 memory footprint 부담을 회피하고 BPF_MAP_TYPE_LRU_HASH 로 두 어 1024 entry 의 단일 instance
 * 만 유지한다. race 안전성 은 inc_flow_bytes 의 __sync_fetch_and_add 로 확보한다. tcp_sendmsg_ret 의
 * ret > 0 분기 와 tcp_cleanup_rbuf 의 copied > 0 분기 에서 5-tuple key 로 본 맵 에 bytes 를 누적 한다.
 * userspace flow.Collector 가 scrape 시점 에 본 맵 을 iterate 해 netobs_flow_bytes_total 로 emit 한다.
 */
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 1024);
    __type(key, struct netobs_flow_key);
    __type(value, struct netobs_flow_value);
} flow_bytes SEC(".maps");

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

/* sock_cgroup_id 는 sock 에 binding 된 cgroup v2 의 inode id 를 반환한다. receive path 의
 * tcp_v4_do_rcv / tcp_rcv_established 는 softirq context 에서 호출되므로
 * bpf_get_current_cgroup_id() 가 인터럽트당한 task (대개 swapper) 의 cgroup 을 가리켜 수신 Pod
 * 식별이 불가하다. 반면 sock 에는 listen / accept 시점에 binding 된 process 의 cgroup 정보가
 * sk_cgrp_data 로 stash 되어 있어 본 helper 로 안전하게 수신 Pod 의 cgroup_id 를 복원할 수 있다.
 * cgroup v1 환경은 본 프로젝트의 cgroup v2 전제와 어긋나므로 별도 fallback 을 두지 않는다. */
static __always_inline __u64 sock_cgroup_id(struct sock *sk)
{
    struct cgroup *cgrp;

    if (!sk)
        return 0;
    cgrp = BPF_CORE_READ(sk, sk_cgrp_data.cgroup);
    if (!cgrp)
        return 0;
    return BPF_CORE_READ(cgrp, kn, id);
}

static __always_inline void fill_conn_from_sock(struct sock *sk, struct netobs_start_info *s)
{
    __u16 family;

    s->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
    s->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
    s->socket_cookie = get_socket_cookie(sk);

    s->ifindex = BPF_CORE_READ(sk, __sk_common.skc_bound_dev_if);
    s->skb_iif = 0;

    /* #103 family enum stash. tcp_sendmsg_ret 에서 sk 가 직접 노출 되지 않 으므로 본 entry 시점
     * 에서 한 번 읽 어 inc_flow_bytes 의 family 가드 입력 으로 사용 한다. NETOBS_AF_INET (2) 또는
     * NETOBS_AF_INET6 (10) 외 family 는 0 으로 둬 inc_flow_bytes 에서 자연 skip 된다. */
    family = BPF_CORE_READ(sk, __sk_common.skc_family);
    s->family = (family == NETOBS_AF_INET || family == NETOBS_AF_INET6) ? (__u8)family : 0;

    /* #103 saddr / daddr 을 family 에 맞춰 채운다. IPv4 는 첫 4 byte 에 skc_rcv_saddr / skc_daddr 를
     * 두고 나머지 12 byte 는 0 으로 초기화. IPv6 는 skc_v6_rcv_saddr / skc_v6_daddr 16 byte 를 그대로
     * 복사. __builtin_memset 으로 zero-init 후 BPF_CORE_READ_INTO 로 IPv4/IPv6 분기 fill. */
    __builtin_memset(s->saddr, 0, NETOBS_ADDR_LEN);
    __builtin_memset(s->daddr, 0, NETOBS_ADDR_LEN);
    if (s->family == NETOBS_AF_INET) {
        __u32 v4_src = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
        __u32 v4_dst = BPF_CORE_READ(sk, __sk_common.skc_daddr);
        __builtin_memcpy(s->saddr, &v4_src, 4);
        __builtin_memcpy(s->daddr, &v4_dst, 4);
    } else if (s->family == NETOBS_AF_INET6) {
        BPF_CORE_READ_INTO(s->saddr, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
        BPF_CORE_READ_INTO(s->daddr, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
    }

    /* #64 drop flow 5-tuple emit 에 protocol 라벨이 필요하다. sk_protocol 은 kernel 의 bitfield 라
     * BPF_CORE_READ_BITFIELD_PROBED 매크로로 안전하게 읽는다. CO-RE 로 kernel 버전 간 layout
     * 차이를 흡수한다. */
    s->protocol = BPF_CORE_READ_BITFIELD_PROBED(sk, sk_protocol);
}

/* fill_tcp_state 는 tcp_sock 의 혼잡 제어 상태 3 종을 netobs_start_info 에 채운다. tcp_sock 은
 * struct sock 을 첫 멤버로 포함하는 derived 타입이라 sock pointer 를 그대로 cast 한다. srtt_us 는
 * kernel 내부적으로 << 3 scale 의 smoothed RTT 라 emit 단계에서 >> 3 해 실제 µs 단위로 변환한다.
 * sk 가 null 이거나 protocol 이 TCP 가 아니면 0 으로 두어 호출자가 stage 마다 일관되게 처리할 수
 * 있게 한다. */
static __always_inline void fill_tcp_state(struct sock *sk, struct netobs_start_info *s)
{
    struct tcp_sock *tp;
    __u32 srtt_scaled;

    if (!sk)
        return;
    if (BPF_CORE_READ_BITFIELD_PROBED(sk, sk_protocol) != IPPROTO_TCP)
        return;

    tp = (struct tcp_sock *)sk;
    s->snd_cwnd     = BPF_CORE_READ(tp, snd_cwnd);
    srtt_scaled     = BPF_CORE_READ(tp, srtt_us);
    s->srtt_us      = srtt_scaled >> 3;
    s->snd_ssthresh = BPF_CORE_READ(tp, snd_ssthresh);
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

/* inc_pod_bytes는 (cgroup_id, direction, layer) 삼중 키로 bytes/packets를 누적한다.
 * BPF_MAP_TYPE_LRU_PERCPU_HASH는 lookup이 per-CPU 슬롯을 반환하므로 atomic 연산 없이 단순 덧셈으로
 * race가 없다. cgroup_id가 0이면 host 작업 또는 cgroup v2 미지원 컨텍스트라 무시한다.
 *
 * packets_delta가 호출자별로 분리된 이유: NIC layer (__dev_queue_xmit/veth_xmit) hook은 1회 호출이
 * 1개 skb와 정확히 대응되어 packets += 1이 패킷 수와 일치한다. 반면 L4 layer (tcp_sendmsg/
 * tcp_cleanup_rbuf) hook은 syscall 또는 cleanup 호출 1회가 여러 TCP segment 또는 read 배치에
 * 대응되므로 packets += 1이 "패킷 수"가 아니라 syscall 수가 되어 메트릭 의미가 깨진다. 따라서 L4
 * 경로는 packets_delta=0으로 호출하고, Collector가 packets==0인 entry는 packets 시리즈를 emit하지
 * 않아 NIC layer만 packets를 노출한다. */
static __always_inline void inc_pod_bytes(__u64 cgroup_id, __u8 direction, __u8 layer,
                                          __u64 bytes_delta, __u64 packets_delta)
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
        val->packets += packets_delta;
        return;
    }

    __builtin_memset(&init, 0, sizeof(init));
    init.bytes   = bytes_delta;
    init.packets = packets_delta;
    bpf_map_update_elem(&pod_bytes, &key, &init, BPF_ANY);
}

static __always_inline void inc_ringbuf_dropped(void)
{
    __u32 key = 0;
    __u64 *v = bpf_map_lookup_elem(&events_dropped, &key);
    if (v)
        (*v)++;        /* percpu 슬롯이라 비-원자 증가로 충분하다 */
}

/* #85 inc_flow_bytes 는 5-tuple key 로 flow_bytes 맵 에 bytes 를 누적 한다. inc_pod_bytes 와 동일
 * 한 lookup-then-update 패턴 이지만 LRU_HASH (non-percpu) 라 race 안전성 을 위해 __sync_fetch_and_add
 * 를 사용 한다. cgroup_id 가 0 이거나 family 가 NETOBS_AF_INET / NETOBS_AF_INET6 가 아닌 socket 은
 * 모두 자동 skip 해 host 작업 과 비-IP flow 가 본 맵 에 등장 하지 않게 한다. 호출자 는
 * fill_conn_from_sock 가 채운 start_info 또는 sk 에서 직접 읽 은 5-tuple 을 그대로 전달 한다.
 *
 * #103 IPv6 확장. saddr / daddr 인자 가 [16]byte 통합 슬롯 으로 변경 되었다. IPv4 호출자 는 첫
 * 4 byte 만 채우고 나머지 12 byte 를 0 으로 두면 IPv6 와 동일 한 key layout 으로 누적 가능 하다.
 */
static __always_inline void inc_flow_bytes(__u64 cgroup_id, __u8 family,
                                           const __u8 *saddr, const __u8 *daddr,
                                           __u16 sport, __u16 dport,
                                           __u8 protocol, __u8 direction,
                                           __u64 bytes_delta)
{
    struct netobs_flow_key key;
    struct netobs_flow_value *val;
    struct netobs_flow_value init;

    if (!cgroup_id || bytes_delta == 0)
        return;
    if (family != NETOBS_AF_INET && family != NETOBS_AF_INET6)
        return;

    __builtin_memset(&key, 0, sizeof(key));
    key.cgroup_id = cgroup_id;
    __builtin_memcpy(key.saddr, saddr, NETOBS_ADDR_LEN);
    __builtin_memcpy(key.daddr, daddr, NETOBS_ADDR_LEN);
    key.sport     = sport;
    key.dport     = dport;
    key.protocol  = protocol;
    key.direction = direction;
    key.family    = family;

    val = bpf_map_lookup_elem(&flow_bytes, &key);
    if (val) {
        __sync_fetch_and_add(&val->bytes, bytes_delta);
        return;
    }

    /* BPF_ANY 로 즉시 갱신 하면 동시 lookup miss 한 두 CPU 가 각자 의 init.bytes 로 덮어 써 한 쪽
     * delta 가 유실 된다. BPF_NOEXIST 로 zero value 만 먼저 등록 한 뒤 다시 lookup 해 atomic add 를
     * 수행 해 첫 패킷 부터 race-safe 누적 을 보장 한다.
     */
    __builtin_memset(&init, 0, sizeof(init));
    bpf_map_update_elem(&flow_bytes, &key, &init, BPF_NOEXIST);
    val = bpf_map_lookup_elem(&flow_bytes, &key);
    if (val)
        __sync_fetch_and_add(&val->bytes, bytes_delta);
}

/* emit_event 의 stack_id 인자는 #83 drop event 의 kernel stack capture 용이다. drop 외 stage 는
 * 항상 -1 을 전달해 비-drop 메트릭 라벨에 stack 차원이 새지 않도록 가드한다. drop emit 만
 * handle_kfree_skb_reason 에서 bpf_get_stackid 의 반환값을 그대로 넘긴다. */
static __always_inline void emit_event(const struct netobs_start_info *s,
                                       __u8 stage,
                                       __u32 reason,
                                       __u32 ret,
                                       __u32 latency_us,
                                       __s32 stack_id)
{
    struct netobs_event *e;

    e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
    if (!e) {
        inc_ringbuf_dropped();
        return;
    }

    e->ts_ns         = bpf_ktime_get_ns();
    e->cgroup_id     = s->cgroup_id;
    e->socket_cookie = s->socket_cookie;
    __builtin_memcpy(e->saddr, s->saddr, NETOBS_ADDR_LEN);
    __builtin_memcpy(e->daddr, s->daddr, NETOBS_ADDR_LEN);
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
    e->protocol      = s->protocol;
    e->family        = s->family;
    e->pad           = 0;

    e->snd_cwnd      = s->snd_cwnd;
    e->srtt_us       = s->srtt_us;
    e->snd_ssthresh  = s->snd_ssthresh;

    e->stack_id      = stack_id;
    e->pad83[0]      = 0;
    e->pad83[1]      = 0;
    e->pad83[2]      = 0;
    e->pad83[3]      = 0;

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
    /* match_target 은 IPv4 전용 테스트 변수다. IPv4 인 경우 첫 4 byte 를 __u32 로 추출 해 매칭 하고
     * IPv6 는 match_target 미적용 (모두 통과). target_daddr 가 0 이면 어떤 family 든 통과. */
    if (s.family == NETOBS_AF_INET) {
        __u32 v4_dst;
        __builtin_memcpy(&v4_dst, s.daddr, 4);
        if (!match_target(v4_dst))
            return 0;
    }

    /* L4 egress 바이트 누적은 kretprobe로 이동했다. entry의 size 인자는 사용자가 send에 넘긴 요청량
     * (헤더 제외)이라 ret로 실제 전송 바이트를 받아 누적해야 partial send/실패(-errno)에서 과대계상이
     * 발생하지 않는다. */

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

    emit_event(s, NETOBS_STAGE_SENDMSG_RET, 0, ret, latency_us, -1);
    s->ret_seen = 1;

    /* L4 egress 바이트는 ret > 0 (실제 전송 바이트) 일 때만 누적한다. ret <= 0 은 -errno 또는
     * 0바이트 전송으로 카운터에 반영하면 안 된다. cgroup_id는 entry 시점에 stash 된 값을 사용해
     * sync syscall 컨텍스트의 task cgroup과 일관성을 유지한다. packets_delta=0인 이유는
     * inc_pod_bytes 주석 참고. */
    if (ret > 0) {
        inc_pod_bytes(s->cgroup_id, NETOBS_DIR_EGRESS, NETOBS_LAYER_L4, (__u64)ret, 0);
        /* #85 5-tuple flow tracker 의 egress 누적. start_info 의 stash 된 5-tuple 과 family 가드 를
         * 그대로 전달 해 IPv4 / IPv6 양쪽 sendmsg 가 본 맵 에 들어가게 한다 (#103). */
        inc_flow_bytes(s->cgroup_id, s->family,
                       s->saddr, s->daddr, s->sport, s->dport,
                       s->protocol, NETOBS_DIR_EGRESS, (__u64)ret);
    }

    if (s->seen_veth && s->seen_devq)
        bpf_map_delete_elem(&starts, &tid);

    return 0;
}

/* #82 tcp_write_xmit 는 TCP send buffer 의 segment 들을 nagle / cwnd / window 제약 아래
 * NIC queue 로 흘려보내는 control path 함수다. entry timestamp 를 starts 에 stash 하고 ret
 * 시점에 latency 를 산정해 control path 의 throttle 비용을 노출한다. seen_write_xmit flag
 * 로 같은 tid 의 nested 또는 반복 호출에서 첫 회 latency 만 측정한다.
 */
SEC("kprobe/tcp_write_xmit")
int BPF_KPROBE(handle_tcp_write_xmit, struct sock *sk)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct netobs_start_info *s;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s || s->seen_write_xmit)
        return 0;

    /* tcp_write_xmit 가 softirq (timer-based retransmit, ack 처리) 컨텍스트에서 호출될 때
     * current task 의 tid 는 인터럽트당한 임의 process 의 tid 를 빌려 쓴다. 그 tid 가 우연히
     * starts 에 entry 를 가진 process 라면 unrelated socket 의 start_info 를 잘못 갱신해 wrong
     * latency 가 emit 된다. sendmsg entry 시점에 stash 된 socket_cookie 와 현재 sk 의 cookie 를
     * 비교해 같은 socket 의 호출만 통과시킨다. cookie 불일치 시 skip 으로 cross-socket race 를
     * 차단한다. */
    if (get_socket_cookie(sk) != s->socket_cookie)
        return 0;

    s->ts_write_xmit = bpf_ktime_get_ns();
    s->seen_write_xmit = 1;
    return 0;
}

SEC("kretprobe/tcp_write_xmit")
int BPF_KRETPROBE(handle_tcp_write_xmit_ret)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct netobs_start_info *s;
    __u64 now;
    __u32 latency_us;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s || !s->ts_write_xmit)
        return 0;

    now = bpf_ktime_get_ns();
    latency_us = (__u32)((now - s->ts_write_xmit) / 1000);

    emit_event(s, NETOBS_STAGE_TCP_WRITE_XMIT, 0, 0, latency_us, -1);
    /* ts_write_xmit 을 0 으로 reset 해 한 sendmsg 사이클 내 첫 회 측정 후 후속 kretprobe 호출
     * (TSO/GSO 다중 segment 또는 timer / ack 콜백 경로) 에서 stale ts 로 잘못된 거대 latency 가
     * emit 되지 않게 한다. seen_write_xmit 은 sendmsg entry 의 zero-init 으로 다음 사이클에서
     * 자연 reset 되므로 본 자리에서는 ts 만 reset 한다.
     */
    s->ts_write_xmit = 0;
    return 0;
}

/* #82 __tcp_transmit_skb 는 개별 segment 단위 transmit entry 다. TSO/GSO 활성 시 단일
 * sendmsg 가 N 회 호출을 트리거하므로 starts map slot race 회피를 위해 첫 segment 만
 * latency 를 측정한다 (seen_transmit flag). 후속 segment 는 stage event 자체를 skip 한다.
 */
SEC("kprobe/__tcp_transmit_skb")
int BPF_KPROBE(handle_tcp_transmit_skb, struct sock *sk, struct sk_buff *skb)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct netobs_start_info *s;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s || s->seen_transmit)
        return 0;

    /* tcp_write_xmit 과 동일 race 가드. __tcp_transmit_skb 가 softirq 컨텍스트에서 호출될 때
     * tid 차용으로 인한 cross-socket race 를 차단한다. socket_cookie 가드가 본 PR 의 self-review
     * 단계에서 발견된 tcp_transmit_skb p99 524ms outlier 의 진짜 원인이며 본 가드로 wrong socket
     * 의 ts set 자체가 막힌다. */
    if (get_socket_cookie(sk) != s->socket_cookie)
        return 0;

    s->ts_transmit_skb = bpf_ktime_get_ns();
    s->seen_transmit = 1;
    return 0;
}

SEC("kretprobe/__tcp_transmit_skb")
int BPF_KRETPROBE(handle_tcp_transmit_skb_ret)
{
    __u32 tid = (__u32)bpf_get_current_pid_tgid();
    struct netobs_start_info *s;
    __u64 now;
    __u32 latency_us;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s || !s->ts_transmit_skb)
        return 0;

    now = bpf_ktime_get_ns();
    latency_us = (__u32)((now - s->ts_transmit_skb) / 1000);

    emit_event(s, NETOBS_STAGE_TCP_TRANSMIT_SKB, 0, 0, latency_us, -1);
    /* tcp_write_xmit_ret 과 동일한 stale ts 가드. ts_transmit_skb 를 0 으로 reset 해 후속
     * kretprobe 호출에서 huge latency (예: 524ms 의 histogram bucket 상한 outlier) 가 emit 되는
     * 회귀를 막는다.
     */
    s->ts_transmit_skb = 0;
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

    emit_event(s, NETOBS_STAGE_TO_VETH, 0, 0, latency_us, -1);
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
    /* NIC layer 는 1 skb = 1 packet 이 자연 대응되어 packets_delta=1 이 정확한 패킷 수다. */
    inc_pod_bytes(s->cgroup_id, NETOBS_DIR_EGRESS, NETOBS_LAYER_NIC, skb_len, 1);

    if (s->seen_devq)
        return 0;

    now = bpf_ktime_get_ns();
    latency_us = (__u32)((now - s->ts_ns) / 1000);

    emit_event(s, NETOBS_STAGE_TO_DEVQ, 0, 0, latency_us, -1);
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

    /* #103 match_target 은 IPv4 전용. IPv6 는 자연 통과. */
    if (s.family == NETOBS_AF_INET) {
        __u32 v4_dst;
        __builtin_memcpy(&v4_dst, s.daddr, 4);
        if (!match_target(v4_dst))
            return 0;
    }

    bpf_get_current_comm(&s.comm, sizeof(s.comm));
    emit_event(&s, NETOBS_STAGE_RETRANS, 0, 0, 0, -1);
    return 0;
}

/* tcp_cleanup_rbuf는 사용자 공간이 TCP 수신 버퍼에서 실제로 읽어간 바이트를 보고하는 시점에
 * 호출된다. recvmsg 경로에서 process context로 실행되어 bpf_get_current_cgroup_id()가 수신 Pod의
 * cgroup_id를 정확히 반환하므로 ingress L4 카운터의 표준 hook으로 적합하다. copied 인자는
 * 음수일 때 에러 신호이므로 양수일 때만 누적한다. NIC layer ingress는 softirq context에서
 * cgroup 해상이 복잡해 본 PR 범위 밖이며 follow-up 이슈에서 다룬다.
 *
 * match_target 필터는 본 hook에서 적용하지 않는다. target_daddr는 egress 흐름의 목적지 IP를
 * 좁히는 테스트용 변수로 ingress에서는 skc_daddr가 원격 peer (송신자) 주소라 같은 변수에 빗대면
 * 의미가 어긋난다. ingress 전수 카운팅이 본 카운터의 의도이며 production에선 target_daddr=""
 * default라 어차피 무영향이다. */
SEC("kprobe/tcp_cleanup_rbuf")
int BPF_KPROBE(handle_tcp_cleanup_rbuf, struct sock *sk, int copied)
{
    __u64 cgroup_id;
    __u16 family_raw;
    __u8  family;
    __u8  saddr[NETOBS_ADDR_LEN] = {0};
    __u8  daddr[NETOBS_ADDR_LEN] = {0};
    __u16 sport, dport;
    __u8 protocol;

    if (copied <= 0)
        return 0;

    cgroup_id = bpf_get_current_cgroup_id();
    if (!cgroup_id)
        return 0;

    /* L4 ingress 바이트만 누적한다. tcp_cleanup_rbuf는 userspace read 1회마다 호출되어 packets
     * 단위와 무관해서 packets_delta=0. inc_pod_bytes 주석 참고. */
    inc_pod_bytes(cgroup_id, NETOBS_DIR_INGRESS, NETOBS_LAYER_L4, (__u64)copied, 0);

    /* #85 5-tuple flow tracker 의 ingress 누적. tcp_cleanup_rbuf 는 sk 를 직접 인자 로 받으므로
     * family 가드 와 5-tuple 추출 을 inline 으로 수행 한다. start_info stash 와 무관 한 별도 path 다.
     * #103 IPv4 / IPv6 양쪽 지원.
     */
    if (!sk)
        return 0;
    family_raw = BPF_CORE_READ(sk, __sk_common.skc_family);
    if (family_raw == NETOBS_AF_INET) {
        __u32 v4_src = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
        __u32 v4_dst = BPF_CORE_READ(sk, __sk_common.skc_daddr);
        __builtin_memcpy(saddr, &v4_src, 4);
        __builtin_memcpy(daddr, &v4_dst, 4);
        family = NETOBS_AF_INET;
    } else if (family_raw == NETOBS_AF_INET6) {
        BPF_CORE_READ_INTO(saddr, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
        BPF_CORE_READ_INTO(daddr, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
        family = NETOBS_AF_INET6;
    } else {
        return 0;
    }
    sport    = BPF_CORE_READ(sk, __sk_common.skc_num);
    dport    = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
    protocol = BPF_CORE_READ_BITFIELD_PROBED(sk, sk_protocol);
    inc_flow_bytes(cgroup_id, family,
                   saddr, daddr, sport, dport, protocol,
                   NETOBS_DIR_INGRESS, (__u64)copied);
    return 0;
}

/* emit_rcv_event 는 receive path 의 sock 기반 stage 진입 시점에 호출되어 5-tuple 과 cgroup_id 를
 * netobs_start_info 로 채워 ringbuf 에 emit 한다. ret / latency_us 는 별도 측정 hook 이 없는 본
 * 진입형 stage 에서 0 으로 둔다. emit 시점의 ts_ns 는 emit_event 가 bpf_ktime_get_ns 로 매번 갱신
 * 하므로 본 helper 가 채운 s->ts_ns 는 ringbuf 에 그대로 흘려보내지 않으며 (현재 Go 측에서 stage 별
 * latency 산정 로직도 두지 않음) emit 시각 자체만 event 라벨로 가공되어 사용된다. sock 에서 family
 * 가 IPv4 가 아니면 5-tuple 라벨 의미가 없어 emit 자체를 skip 한다. */
static __always_inline void emit_rcv_event(struct sock *sk, struct sk_buff *skb, __u8 stage)
{
    struct netobs_start_info s = {};
    __u64 pid_tgid;
    __u16 family;

    if (!sk)
        return;

    family = BPF_CORE_READ(sk, __sk_common.skc_family);
    if (family != NETOBS_AF_INET && family != NETOBS_AF_INET6)
        return;

    pid_tgid     = bpf_get_current_pid_tgid();
    s.ts_ns      = bpf_ktime_get_ns();
    s.cgroup_id  = sock_cgroup_id(sk);
    s.pid        = pid_tgid >> 32;
    s.tid        = (__u32)pid_tgid;

    fill_conn_from_sock(sk, &s);
    /* fill_conn_from_sock 는 send path 기준으로 saddr=local / daddr=remote 를 채운다. receive
     * path 의 ingress event 는 흐름의 source 가 remote peer 이고 destination 이 local Pod 이므로
     * 양쪽을 swap 해 downstream 라벨 (src=*, dst=*) 의 의미가 send path 와 일관되게 한다.
     * #103 IPv6 확장 으로 [16]byte 통합 슬롯 의 swap 도 동일 패턴 으로 처리. */
    {
        __u8  tmp_addr[NETOBS_ADDR_LEN];
        __u16 tmp_port = s.sport;
        __builtin_memcpy(tmp_addr, s.saddr, NETOBS_ADDR_LEN);
        __builtin_memcpy(s.saddr, s.daddr, NETOBS_ADDR_LEN);
        __builtin_memcpy(s.daddr, tmp_addr, NETOBS_ADDR_LEN);
        s.sport = s.dport;
        s.dport = tmp_port;
    }
    fill_tcp_state(sk, &s);
    if (skb)
        fill_dev_from_skb(skb, &s);

    bpf_get_current_comm(&s.comm, sizeof(s.comm));
    emit_event(&s, stage, 0, 0, 0, -1);
}

/* #65 receive path stage 별 kprobe.
 *
 * tcp_v4_rcv 는 L3 entry 로 socket lookup 이전이라 sk 가 null 이며 Pod 귀속이 불가능해 본 커밋에서는
 * emit 을 보류한 채 attach 만 유지한다. 후속 follow-up 에서 skb 헤더 파싱으로 5-tuple 을 복원해
 * stage 카운터로 활용할 여지를 남긴다. tcp_v4_do_rcv 부터는 sock 인자가 있어 sk_cgrp_data 기반
 * cgroup_id 로 수신 Pod 를 정확히 식별한다. tcp_recvmsg 는 process context 라
 * bpf_get_current_cgroup_id() 도 정답을 주지만 sock 경로로 통일해 다른 stage 와 동일한 키 공간을
 * 유지한다. netif_receive_skb 와 sk_data_ready 는 본 PR 의 범위 검토에서 제외했다 (cgroup 미식별,
 * kernel 6.x inline). */
SEC("kprobe/tcp_v4_rcv")
int BPF_KPROBE(handle_tcp_v4_rcv)
{
    return 0;
}

SEC("kprobe/tcp_v4_do_rcv")
int BPF_KPROBE(handle_tcp_v4_do_rcv, struct sock *sk, struct sk_buff *skb)
{
    emit_rcv_event(sk, skb, NETOBS_STAGE_RCV_DEMUX);
    return 0;
}

/* #103 IPv6 TCP receive path entry. tcp_v4_rcv 와 동일 패턴 의 stub 으로 두며 RCV_L3 stage 의 event
 * emit 은 본 hook 에서 수행 하지 않는다 (cgroup 미식별 시점). tcp_v6_do_rcv 에서 sock 기반 demux
 * stage 가 emit_rcv_event 로 처리 된다. */
SEC("kprobe/tcp_v6_rcv")
int BPF_KPROBE(handle_tcp_v6_rcv)
{
    return 0;
}

/* #103 IPv6 TCP demux. tcp_v4_do_rcv 와 동일 시그니처. emit_rcv_event 가 family 분기 로 IPv6
 * 흐름 도 자연 capture 한다. */
SEC("kprobe/tcp_v6_do_rcv")
int BPF_KPROBE(handle_tcp_v6_do_rcv, struct sock *sk, struct sk_buff *skb)
{
    emit_rcv_event(sk, skb, NETOBS_STAGE_RCV_DEMUX);
    return 0;
}

SEC("kprobe/tcp_rcv_established")
int BPF_KPROBE(handle_tcp_rcv_established, struct sock *sk, struct sk_buff *skb)
{
    emit_rcv_event(sk, skb, NETOBS_STAGE_RCV_ESTABLISHED);
    return 0;
}

SEC("kprobe/tcp_recvmsg")
int BPF_KPROBE(handle_tcp_recvmsg, struct sock *sk)
{
    emit_rcv_event(sk, NULL, NETOBS_STAGE_RCV_APP);
    return 0;
}

SEC("kprobe/kfree_skb_reason")
int BPF_KPROBE(handle_kfree_skb_reason, struct sk_buff *skb, int reason)
{
    struct sock *sk;
    struct netobs_start_info s = {};
    __u64 pid_tgid = bpf_get_current_pid_tgid();
    __u16 family;
    __s32 stack_id;

    sk = BPF_CORE_READ(skb, sk);
    if (!sk)
        return 0;

    /* #64 의 drop flow 5-tuple 은 IPv4 한정 첫 구현이다. NETOBS_AF_INET 외의 family (AF_INET6 등)
     * 는 5-tuple 라벨 셋이 의미가 없어 본 drop event 의 emit 자체를 skip 한다. */
    family = BPF_CORE_READ(sk, __sk_common.skc_family);
    if (family != NETOBS_AF_INET)
        return 0;

    s.ts_ns     = bpf_ktime_get_ns();
    s.cgroup_id = bpf_get_current_cgroup_id();
    s.pid       = pid_tgid >> 32;
    s.tid       = (__u32)pid_tgid;

    fill_conn_from_sock(sk, &s);
    fill_dev_from_skb(skb, &s);

    /* #103 match_target 은 IPv4 전용. IPv6 는 자연 통과. */
    if (s.family == NETOBS_AF_INET) {
        __u32 v4_dst;
        __builtin_memcpy(&v4_dst, s.daddr, 4);
        if (!match_target(v4_dst))
            return 0;
    }

    /* #83 drop event 의 kernel stack capture. BPF_F_FAST_STACK_CMP 는 stack id 산정에 frame
     * pointer 비교가 아닌 빠른 hash 기반 비교를 사용해 hot path 비용을 최소화한다. ctx 는 kprobe 의
     * 호출 시점 register frame 이라 race 가드가 별도 필요하지 않다. 실패 시 -EFAULT 등 음수를 반환
     * 하며 userspace resolver 는 stack_id < 0 인 event 의 stack 메트릭 emit 을 skip 한다. */
    stack_id = bpf_get_stackid(ctx, &drop_stacks, BPF_F_FAST_STACK_CMP);

    bpf_get_current_comm(&s.comm, sizeof(s.comm));
    emit_event(&s, NETOBS_STAGE_DROP, reason, 0, 0, stack_id);
    return 0;
}
