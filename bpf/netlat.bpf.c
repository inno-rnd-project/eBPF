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

/* #121 TSO/GSO send path segment 누적 latency map. key 는 socket_cookie (u64), value 는 첫 transmit
 * timestamp 와 segment 단위 latency 누적 합산 과 segment 개수 의 누적 추적 struct 다. sendmsg
 * 사이클 중 호출 되는 모든 __tcp_transmit_skb 의 segment latency 를 본 map 에 합산 누적 하고
 * sendmsg_ret 시점 에 emit 후 entry 를 cleanup 한다. max_entries 8192 는 (활성 socket 4096) x
 * (direction 2) 의 cap 예산. BPF_MAP_TYPE_LRU_HASH 라 entry 폭주 시 자연 evict 된다.
 */
struct netobs_seg_accum {
    __u64 first_ts;
    __u64 cumulative_latency_ns;
    __u32 segment_count;
    __u8  pad[4];
};

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 8192);
    __type(key, __u64);
    __type(value, struct netobs_seg_accum);
} seg_accum SEC(".maps");

/* #141 receive path stage 별 latency 측정 map. key 는 socket_cookie (u64), value 는 L3 진입 시각과
 * established 처리 시각 두 timestamp 다. send path 의 starts (tid 키) 와 달리 receive path 는 softirq
 * context 라 tid 가 무의미 하므로 socket_cookie 로 동일 connection 의 stage 진입 시점 을 묶는다.
 * tcp_v4_rcv 가 ts_l3 를 stash 하고 tcp_v4_do_rcv / tcp_rcv_established 가 ts_l3 기준 누적 latency 를
 * 산정 한다 (send path 의 to_devq / to_veth 와 동일 누적 패턴). tcp_rcv_established 가 ts_established 를
 * 갱신 하고 tcp_recvmsg 가 그 기준 으로 app pickup 대기 latency 를 산정 후 entry 를 cleanup 한다.
 * max_entries 8192 는 seg_accum 과 동일한 (활성 socket 4096) x 2 예산 이며 LRU 라 자연 evict 된다. */
struct netobs_recv_state {
    __u64 ts_l3;
    __u64 ts_established;
    /* #197 첫 미-ACK 데이터 수신 시각. tcp_rcv_established 가 (0 일 때만) stamp 하고 tcp_send_ack 가
     * ACK 송신 시점 과의 차분 을 "수신측 ACK 대기" latency 로 채택 후 0 으로 리셋 한다. 0 은 미상관 (직전
     * ACK 이후 신규 데이터 없음 / standalone ACK) 을 뜻해 spurious 0 sample emit 을 막는 데도 쓰인다. */
    __u64 ts_data;
};

/* #141 receive path stage latency 의 stale entry 가드 임계 (nanoseconds). socket_cookie 는 socket
 * lifetime 동안 unique 하나 socket 종료 후 또는 LRU evict 후 재할당 될 수 있어, 닫힌 socket 의 오래된
 * ts_established 가 남아 있으면 now 와의 차분이 거대한 outlier 가 된다. 정상 app pickup 대기는 본 임계
 * (10s) 를 넘지 않으므로 초과 차분은 stale entry 로 간주해 latency 를 채택 하지 않는다. */
#define NETOBS_RCV_STALE_NS 10000000000ULL

/* #141 RCV_DEMUX 의 L3→demux recent 가드 임계 (nanoseconds). tcp_v4_rcv 의 L3 진입 과 tcp_v4_do_rcv
 * 는 동일 softirq 연속 이라 차분 이 µs scale 이다. cross-node 처럼 early demux 가 없어 이전 do_rcv 의
 * ts_l3 가 남아 있는 경우 차분 이 패킷 간 간격 (수 ms ~ 수 초) 이 되어 커널 처리시간 으로 오측정 되므로,
 * 본 임계 (1ms) 초과 차분 은 L3→demux 가 아닌 stale 로 보고 RCV_DEMUX latency 를 0 으로 둔다. */
#define NETOBS_RCV_DEMUX_MAX_NS 1000000ULL

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __uint(max_entries, 8192);
    __type(key, __u64);
    __type(value, struct netobs_recv_state);
} recv_starts SEC(".maps");

/* #173 NIC ingress→L3 구간 의 skb->protocol 필터 상수. NIC 진입 stash 를 IP / IPv6 패킷 으로만
 * 한정 해 ARP 등 비-IP 트래픽 의 불필요 한 per-CPU slot 갱신 을 피한다. skb->protocol 은 __be16 라
 * host 상수 를 bpf_htons 로 변환 해 비교 한다. */
#define NETOBS_ETH_P_IP   0x0800
#define NETOBS_ETH_P_IPV6 0x86DD

/* #173 NIC ingress→L3 구간 의 sanity 상한 (nanoseconds). 본 구간 은 동일 CPU 의 단일 softirq 콜체인
 * (__netif_receive_skb → ip_rcv → tcp_v4_rcv) 으로 직렬 진행 되어 µs scale 이다. nic_ingress 의 skb
 * 포인터 일치 로 동일 패킷 을 보장 하므로 stale 차단 보다는 skb 포인터 재사용 등 극단 outlier 가드
 * 목적 이며, 1ms 초과 차분 은 콜체인 segment 가 아닌 artifact 로 보고 latency 를 채택 하지 않는다. */
#define NETOBS_RCV_NIC_MAX_NS 1000000ULL

/* #173 NIC ingress→L3 구간 측정용 per-CPU 단일 slot. __netif_receive_skb 는 softirq context 라 수신
 * skb 의 socket / cgroup 이 아직 미해상 이므로 socket_cookie 키 stash 가 불가능 하다. 대신 수신 처리 가
 * 동일 CPU 의 단일 softirq 콜체인 (__netif_receive_skb → tcp_v4_rcv → tcp_v4_do_rcv) 으로 직렬 진행
 * 되는 점 을 활용 한다. NIC 진입 시각 (ts_nic) 과 skb 포인터 를 __netif_receive_skb 에서 stash 하고,
 * L3 진입 (tcp_v4_rcv / tcp_v6_rcv) 에서 skb 포인터 일치 시 진입 시각 (ts_l3) 을 stamp 한다. Pod 귀속 은
 * socket 미해상 단계 라, sock 인자 가 보장 되는 demux (tcp_v4_do_rcv) 에서 slot 의 두 timestamp 로
 * NIC→L3 latency 를 산정 해 emit 한다. 두 timestamp 가 모두 동기 콜체인 에서 찍히므로 demux 가 backlog
 * 로 지연 emit 되어도 latency 값 은 정확 하다. skb 포인터 일치 검사 가 RPS / loopback 등 콜체인 이 끊긴
 * 경우 의 오측정 을 차단 한다 (dev 실측 skb 정합 98.6%). */
struct netobs_nic_ingress {
    __u64 ts_nic;   /* __netif_receive_skb 진입 시각 */
    __u64 ts_l3;    /* tcp_v4_rcv / tcp_v6_rcv (L3 진입) 시각, 동일 skb 일 때만 stamp */
    __u64 skb;      /* 상관 대상 skb 포인터 */
};

struct {
    __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct netobs_nic_ingress);
} nic_ingress SEC(".maps");

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
        /* #103 BPF_CORE_READ_INTO 의 ___read 매크로 가 sizeof(*(dst)) 를 쓰므로 __u8 array 인자
         * (s->saddr) 는 *(__u8*) == 1 byte 만 복사 되는 버그 가 있다. bpf_core_read 를 직접 호출 해
         * sizeof(s->saddr) == 16 byte 명시 복사. */
        bpf_core_read(s->saddr, sizeof(s->saddr), &sk->__sk_common.skc_v6_rcv_saddr);
        bpf_core_read(s->daddr, sizeof(s->daddr), &sk->__sk_common.skc_v6_daddr);
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
/* #103 IPv6 비-routable 주소 필터. link-local (fe80::/10) 과 multicast (ff00::/8) 와 loopback (::1)
 * 을 BPF 단 에서 zero-cost skip. cluster 자체 트래픽 (CoreDNS link-local 등) 이 flow_bytes 의 cardinality
 * 를 폭증 시키지 않도록 차단 한다. IPv4 의 동등 필터 는 별도 follow-up 으로 위임. */
static __always_inline int ipv6_is_filtered(const __u8 *addr)
{
    /* link-local fe80::/10: 첫 byte 0xfe, 둘째 byte 상위 2 bit 가 10 (mask 0xc0 = 0x80). */
    if (addr[0] == 0xfe && (addr[1] & 0xc0) == 0x80)
        return 1;
    /* multicast ff00::/8: 첫 byte 0xff. */
    if (addr[0] == 0xff)
        return 1;
    /* loopback ::1: 첫 15 byte 가 0 이고 16 byte 가 1. BPF verifier 의 back-edge 분석 부담 회피 와
     * kernel 별 loop 지원 차이 흡수 를 위해 #pragma unroll 로 명시 언롤. */
    #pragma unroll
    for (int i = 0; i < 15; i++) {
        if (addr[i] != 0)
            return 0;
    }
    if (addr[15] == 0x01)
        return 1;
    return 0;
}

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
    /* #103 IPv6 link-local / multicast / loopback 자동 skip. */
    if (family == NETOBS_AF_INET6) {
        if (ipv6_is_filtered(saddr) || ipv6_is_filtered(daddr))
            return;
    }

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
 * handle_kfree_skb_reason 에서 bpf_get_stackid 의 반환값을 그대로 넘긴다. #121 의 full_latency_ns
 * 와 segment_count 인자 는 sendmsg_ret stage 에서만 0 이 아닌 값을 전달 하며 다른 stage 는 모두
 * 0 으로 채워 라벨 의미 정합 을 유지 한다. */
static __always_inline void emit_event(const struct netobs_start_info *s,
                                       __u8 stage,
                                       __u32 reason,
                                       __u32 ret,
                                       __u32 latency_us,
                                       __s32 stack_id,
                                       __u64 full_latency_ns,
                                       __u32 segment_count)
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

    e->full_latency_ns = full_latency_ns;
    e->segment_count   = segment_count;
    e->pad121[0]       = 0;
    e->pad121[1]       = 0;
    e->pad121[2]       = 0;
    e->pad121[3]       = 0;

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

    /* #121 sendmsg 사이클 의 segment 누적 latency 와 segment_count 를 sendmsg_ret stage event 에 함께
     * emit. seg_accum entry 가 부재 한 경우 (tcp_transmit_skb 미호출 sendmsg 등) 0/0 으로 emit. emit
     * 직후 cleanup 으로 long-running socket 의 stale entry 방지. */
    __u64 full_latency_ns = 0;
    __u32 segment_count = 0;
    struct netobs_seg_accum *seg = bpf_map_lookup_elem(&seg_accum, &s->socket_cookie);
    if (seg) {
        full_latency_ns = seg->cumulative_latency_ns;
        segment_count = seg->segment_count;
        bpf_map_delete_elem(&seg_accum, &s->socket_cookie);
    }

    emit_event(s, NETOBS_STAGE_SENDMSG_RET, 0, ret, latency_us, -1, full_latency_ns, segment_count);
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

    emit_event(s, NETOBS_STAGE_TCP_WRITE_XMIT, 0, 0, latency_us, -1, 0, 0);
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
    __u64 now;

    s = bpf_map_lookup_elem(&starts, &tid);
    if (!s)
        return 0;

    /* tcp_write_xmit 과 동일 race 가드. __tcp_transmit_skb 가 softirq 컨텍스트에서 호출될 때
     * tid 차용으로 인한 cross-socket race 를 차단한다. socket_cookie 가드가 본 PR 의 self-review
     * 단계에서 발견된 tcp_transmit_skb p99 524ms outlier 의 진짜 원인이며 본 가드로 wrong socket
     * 의 ts set 자체가 막힌다. */
    if (get_socket_cookie(sk) != s->socket_cookie)
        return 0;

    now = bpf_ktime_get_ns();

    /* #82 첫 segment 의 ts_transmit_skb 만 stage_latency emit 용으로 보존. seen_transmit flag 가드로
     * 후속 segment 는 stage_latency emit 흐름에서 자연 제외 된다. */
    if (!s->seen_transmit) {
        s->ts_transmit_skb = now;
        s->seen_transmit = 1;
    }

    /* #121 모든 segment 의 entry timestamp 갱신. kretprobe 의 segment 단위 latency 산정 에 사용. */
    s->ts_segment_entry = now;

    /* #121 seg_accum 에 socket_cookie 별 segment count 증가. 첫 segment 는 entry create, 후속 segment
     * 는 count++. cumulative_latency 는 kretprobe 에서 누적. */
    struct netobs_seg_accum *seg = bpf_map_lookup_elem(&seg_accum, &s->socket_cookie);
    if (!seg) {
        struct netobs_seg_accum init = {
            .first_ts = now,
            .cumulative_latency_ns = 0,
            .segment_count = 1,
        };
        bpf_map_update_elem(&seg_accum, &s->socket_cookie, &init, BPF_ANY);
    } else {
        seg->segment_count++;
    }
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
    if (!s)
        return 0;

    now = bpf_ktime_get_ns();

    /* #121 모든 segment 의 ret 시점에 segment latency 를 seg_accum 에 누적. ts_segment_entry 는 매
     * segment entry 마다 갱신되어 stale ts 위험 zero. */
    if (s->ts_segment_entry) {
        __u64 seg_lat = now - s->ts_segment_entry;
        struct netobs_seg_accum *seg = bpf_map_lookup_elem(&seg_accum, &s->socket_cookie);
        if (seg)
            seg->cumulative_latency_ns += seg_lat;
        s->ts_segment_entry = 0;
    }

    /* #82 첫 segment 의 stage_latency 만 emit. seen_transmit 가드로 ts_transmit_skb 가 살아 있는
     * 첫 사이클 만 통과 한다. */
    if (s->ts_transmit_skb) {
        latency_us = (__u32)((now - s->ts_transmit_skb) / 1000);
        emit_event(s, NETOBS_STAGE_TCP_TRANSMIT_SKB, 0, 0, latency_us, -1, 0, 0);
        /* tcp_write_xmit_ret 과 동일한 stale ts 가드. ts_transmit_skb 를 0 으로 reset 해 후속
         * kretprobe 호출에서 huge latency (예: 524ms 의 histogram bucket 상한 outlier) 가 emit 되는
         * 회귀를 막는다.
         */
        s->ts_transmit_skb = 0;
    }
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

    emit_event(s, NETOBS_STAGE_TO_VETH, 0, 0, latency_us, -1, 0, 0);
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

    emit_event(s, NETOBS_STAGE_TO_DEVQ, 0, 0, latency_us, -1, 0, 0);
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
    emit_event(&s, NETOBS_STAGE_RETRANS, 0, 0, 0, -1, 0, 0);
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
        /* #103 BPF_CORE_READ_INTO 의 sizeof(*(dst)) 가 array decay 후 1 byte 라 16 byte 복사 가
         * 깨지는 버그 회피. bpf_core_read 로 sizeof(saddr) == 16 명시. */
        bpf_core_read(saddr, sizeof(saddr), &sk->__sk_common.skc_v6_rcv_saddr);
        bpf_core_read(daddr, sizeof(daddr), &sk->__sk_common.skc_v6_daddr);
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

/* #173 capture_nic_l3 는 L3 진입 (tcp_v4_rcv / tcp_v6_rcv) 시점 에 per-CPU nic_ingress slot 의 ts_l3 를
 * stamp 한다. socket lookup 이전 단계 라 sk 가 없어도 동작 하며 (NIC→L3 구간 은 Pod 귀속 과 무관 하게
 * 시각 만 필요), __netif_receive_skb 가 stash 한 skb 포인터 와 현재 skb 가 일치 할 때만 stamp 해 동일
 * 패킷 임을 보장 한다. 실제 latency 산정 과 emit 은 sock 인자 가 보장 되는 tcp_v4_do_rcv 에서 수행 된다. */
static __always_inline void capture_nic_l3(struct sk_buff *skb)
{
    __u32 key = 0;
    struct netobs_nic_ingress *ni;

    if (!skb)
        return;
    ni = bpf_map_lookup_elem(&nic_ingress, &key);
    if (!ni)
        return;
    if (ni->skb == (__u64)(unsigned long)skb && ni->ts_nic)
        ni->ts_l3 = bpf_ktime_get_ns();
}

/* #141 stash_recv_l3 는 tcp_v4_rcv / tcp_v6_rcv 의 L3 진입 시점 timestamp 를 socket_cookie 키로
 * recv_starts 에 기록한다. tcp_v4_rcv 는 socket lookup 이전 단계라 sk 인자가 없어 skb->sk (early
 * demux 결과) 로 sock 을 복원한다. established 연결은 early demux 로 skb->sk 가 채워져 있어
 * socket_cookie 산정이 가능하고, 신규 SYN / listen socket 등 sk 가 null 인 케이스는 skip 해
 * RCV_DEMUX 가 ts_l3 부재 시 latency 0 으로 자연 fallback 하게 한다. */
static __always_inline void stash_recv_l3(struct sk_buff *skb)
{
    struct sock *sk;
    struct netobs_recv_state *st;
    __u64 cookie;
    __u64 now;

    if (!skb)
        return;
    sk = BPF_CORE_READ(skb, sk);
    if (!sk)
        return;
    cookie = get_socket_cookie(sk);
    if (!cookie)
        return;

    now = bpf_ktime_get_ns();
    /* 기존 entry 가 있으면 RCV_ESTABLISHED 가 기록한 ts_established 를 보존하고 ts_l3 만 갱신한다.
     * value 전체를 0 초기화 한 struct 로 덮어쓰면 고트래픽 환경 에서 app read (RCV_APP) 이전 에
     * 도착한 다음 패킷의 stash 가 ts_established 를 0 으로 날려 RCV_APP latency 가 0 으로 빈번히
     * 폴백 되는 문제가 발생 한다. */
    st = bpf_map_lookup_elem(&recv_starts, &cookie);
    if (st) {
        st->ts_l3 = now;
        return;
    }
    {
        struct netobs_recv_state ns = {};
        ns.ts_l3 = now;
        bpf_map_update_elem(&recv_starts, &cookie, &ns, BPF_ANY);
    }
}

/* emit_rcv_event 는 receive path 의 sock 기반 stage 진입 시점에 호출되어 5-tuple 과 cgroup_id 를
 * netobs_start_info 로 채워 ringbuf 에 emit 한다. #141 부터 socket_cookie 기준 recv_starts 맵 으로
 * stage 별 누적 latency 를 산정해 latency_us 로 함께 emit 한다. RCV_DEMUX 와 RCV_ESTABLISHED 는
 * tcp_v4_rcv 가 stash 한 ts_l3 기준 누적 (send path 의 to_devq 와 동일 패턴) 이라 두 stage 차가
 * demux→established 커널 처리 비용 이고, RCV_APP 은 RCV_ESTABLISHED 가 갱신한 ts_established 기준
 * app pickup 대기 다. sock 에서 family 가 IPv4 / IPv6 가 아니면 5-tuple 라벨 의미가 없어 emit 자체를
 * skip 한다. */
static __always_inline void emit_rcv_event(struct sock *sk, struct sk_buff *skb, __u8 stage)
{
    struct netobs_start_info s = {};
    struct netobs_recv_state *st;
    __u64 pid_tgid;
    __u64 now;
    __u16 family;
    __u32 latency_us = 0;
    __u8  nic_matched = 0;   /* #173 RCV_NIC 의 nic_ingress 상관 성공 여부. latency_us==0 (sub-µs) 와
                              * 미상관 을 구분 해 sub-µs 측정 값 을 버리지 않게 한다. */

    if (!sk)
        return;

    family = BPF_CORE_READ(sk, __sk_common.skc_family);
    if (family != NETOBS_AF_INET && family != NETOBS_AF_INET6)
        return;

    pid_tgid     = bpf_get_current_pid_tgid();
    now          = bpf_ktime_get_ns();
    s.ts_ns      = now;
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

    /* #141 socket_cookie 기준 stage latency 산정. fill_conn_from_sock 가 s.socket_cookie 를 채운
     * 뒤이므로 본 시점에 recv_starts lookup 이 가능하다. monotonic clock 이라 now 가 stash 시각보다
     * 앞설 일은 없으나, cookie 재할당 으로 닫힌 socket 의 stale entry 를 조회 하면 차분 이 거대한
     * outlier 가 되므로 NETOBS_RCV_STALE_NS (10s) 초과 차분 은 채택 하지 않고 기준점 을 리셋 한다. */
    st = bpf_map_lookup_elem(&recv_starts, &s.socket_cookie);
    if (stage == NETOBS_STAGE_RCV_DEMUX) {
        if (st) {
            /* early demux 로 tcp_v4_rcv 가 직전에 ts_l3 를 stash 한 경우만 L3→demux 를 측정 한다.
             * do_rcv 진입 시점 을 RCV_ESTABLISHED 기준점 으로 매번 재설정 해, cross-node 처럼
             * tcp_v4_rcv stash 가 없어 이전 do_rcv 의 ts_l3 가 남아 있는 경우 패킷 간 간격 이 latency
             * 로 오측정 되는 것을 막는다. L3→demux 는 µs scale 이라 NETOBS_RCV_DEMUX_MAX_NS (1ms)
             * 초과 차분 은 stale 로 보고 0 으로 둔다. ts_established 는 RCV_ESTABLISHED / RCV_APP 의
             * app pickup 측정 기준 이라 본 분기 에서 건드리지 않는다. */
            if (st->ts_l3 && now > st->ts_l3 && (now - st->ts_l3) < NETOBS_RCV_DEMUX_MAX_NS)
                latency_us = (__u32)((now - st->ts_l3) / 1000);
            st->ts_l3 = now;
        } else {
            /* tcp_v4_rcv 의 early demux stash 실패 (skb->sk null, cross-node forwarding / tunnel
             * 경로 등) 시 do_rcv 를 RX 기준점 으로 생성 해 후속 RCV_ESTABLISHED 와 RCV_APP latency
             * 측정 을 보장 한다. 이 경우 RCV_DEMUX 자신 은 기준점 이라 latency 0 으로 emit 된다. */
            struct netobs_recv_state ns = {};
            ns.ts_l3 = now;
            bpf_map_update_elem(&recv_starts, &s.socket_cookie, &ns, BPF_ANY);
        }
    } else if (stage == NETOBS_STAGE_RCV_ESTABLISHED) {
        if (st && st->ts_l3 && now > st->ts_l3) {
            __u64 diff = now - st->ts_l3;
            if (diff < NETOBS_RCV_STALE_NS)
                latency_us = (__u32)(diff / 1000);
        }
        if (st) {
            st->ts_established = now;
            /* #197 직전 ACK 이후 첫 데이터 수신 시각 을 기준점 으로 기록 해, 지연 ACK 가 여러 세그먼트 를
             * 누적 ACK 할 때 "첫 미-ACK 데이터 → ACK 송신" 대기 를 측정 한다. tcp_send_ack 가 채택 후 0 으로
             * 리셋 하므로 다음 데이터 수신 이 다시 기준점 이 된다. 양방향 흐름 에서 ACK 가 데이터 에 piggyback
             * 되어 tcp_send_ack 를 안 타면 기준점 이 리셋 되지 않는데, 이때 기준점 이 stale (>10s) 해진 소켓 의
             * 후속 계측 이 영구 누락 되지 않도록 0 또는 stale 일 때 모두 현재 시각 으로 재기록 한다 (monotonic
             * 역행 now < ts_data 도 방어적 재기록). */
            if (!st->ts_data || now < st->ts_data || (now - st->ts_data) >= NETOBS_RCV_STALE_NS)
                st->ts_data = now;
        }
    } else if (stage == NETOBS_STAGE_ACK_WAIT) {
        /* #197 수신측 ACK 대기 = 첫 미-ACK 데이터 수신 (ts_data) 부터 ACK 송신 (now) 까지. ts_data 미상관
         * (recv_starts entry 부재 / 직전 ACK 이후 신규 데이터 없음) 이거나 stale (>10s) 이면 histogram 에
         * spurious 0 sample 을 넣지 않도록 emit 을 skip 한다 (rcv_nic 미상관 skip 과 동일 논리). 채택 시 ts_data
         * 를 0 으로 리셋 해 다음 데이터 수신 이 새 기준점 이 되게 한다. */
        if (!st || !st->ts_data || now <= st->ts_data || (now - st->ts_data) >= NETOBS_RCV_STALE_NS)
            return;
        latency_us = (__u32)((now - st->ts_data) / 1000);
        st->ts_data = 0;
    } else if (stage == NETOBS_STAGE_RCV_APP) {
        if (st && st->ts_established && now > st->ts_established) {
            __u64 diff = now - st->ts_established;
            if (diff < NETOBS_RCV_STALE_NS)
                latency_us = (__u32)(diff / 1000);
        }
        if (st)
            bpf_map_delete_elem(&recv_starts, &s.socket_cookie);
    } else if (stage == NETOBS_STAGE_RCV_NIC) {
        /* #173 NIC ingress→L3 구간. per-CPU nic_ingress 의 NIC 진입 시각 (ts_nic) 과 L3 진입 시각 (ts_l3)
         * 이 모두 stamp 되고 현재 demux skb 와 slot 의 skb 포인터 가 일치 (동일 softirq 콜체인) 할 때만 두
         * 시각 의 차분 을 NIC→L3 latency 로 채택 한다. 두 시각 모두 동기 콜체인 에서 찍히므로 demux 가
         * backlog 로 지연 emit 되어도 값 은 정확 하다. 채택 후 slot 의 skb 를 0 으로 비워 후속 패킷 이
         * 동일 slot 의 stale 값 을 중복 채택 하지 않게 한다. NIC→L3 는 동기 콜체인 의 µs scale 구간 이라
         * 차분 이 1µs 미만 이면 latency_us 가 0 으로 내림 되는데, 이는 정상 측정 값 이므로 nic_matched
         * flag 로 미상관 (skip 대상) 과 구분 한다. skb 가 null 이면 consume 후 ni->skb 가 0 인 slot 과
         * 0 == 0 으로 오매칭 되어 stale 값 을 채택 할 수 있으므로 skb 자체 의 non-null 을 먼저 가드 한다.
         * 가상화 / 저해상 클럭 환경 에서 ts_l3 와 ts_nic 가 동일 ns 일 수 있는데 0ns 도 정상 측정 이라
         * ts_l3 >= ts_nic 로 수집 한다 (등호 누락 시 0µs sample 손실). */
        __u32 nkey = 0;
        struct netobs_nic_ingress *ni = bpf_map_lookup_elem(&nic_ingress, &nkey);
        if (skb && ni && ni->skb == (__u64)(unsigned long)skb && ni->ts_nic && ni->ts_l3 &&
            ni->ts_l3 >= ni->ts_nic && (ni->ts_l3 - ni->ts_nic) < NETOBS_RCV_NIC_MAX_NS) {
            latency_us = (__u32)((ni->ts_l3 - ni->ts_nic) / 1000);
            ni->skb = 0;
            nic_matched = 1;
        }
    }

    /* #173 RCV_NIC 는 per-CPU nic_ingress 와 현재 skb 가 일치 해 NIC→L3 구간 이 실제 산정 된 경우 만
     * emit 한다. 콜체인 이 끊긴 (RPS / loopback / backlog 지연) 미상관 패킷 까지 emit 하면 histogram 에
     * spurious 0 sample 이 누적 되므로 nic_matched 가 0 이면 emit 을 skip 한다. latency_us 자체 가 0 인
     * 경우 (sub-µs 구간) 는 정상 측정 이라 emit 한다. 다른 rcv stage 는 ts_l3 부재 시 0 emit 이 의미 있는
     * fallback (#141) 이라 본 guard 를 적용 하지 않는다. */
    if (stage == NETOBS_STAGE_RCV_NIC && !nic_matched)
        return;

    bpf_get_current_comm(&s.comm, sizeof(s.comm));
    emit_event(&s, stage, 0, 0, latency_us, -1, 0, 0);
}

/* #173 __netif_receive_skb 는 NIC 드라이버 가 skb 를 커널 stack 에 올리는 보편 진입점 이다. 공개
 * 심볼 netif_receive_skb 는 NAPI / backlog 경로 에 우회 되어 거의 발화 하지 않으므로 (dev 실측 1 회),
 * 모든 ingress 가 지나는 __netif_receive_skb 를 hook 한다 (실측 2351 회 로 tcp_v4_rcv 1815 회 를 포함).
 * softirq context 라 socket / cgroup 미해상 이므로 NIC 진입 시각 과 skb 포인터 만 per-CPU slot 에 stash
 * 하고 ts_l3 는 0 으로 리셋 한다. L3 진입 시각 stamp 와 Pod 귀속 / emit 은 후속 stage 로 미룬다. IP /
 * IPv6 패킷 만 stash 해 비-IP 트래픽 의 불필요 한 slot 갱신 을 피한다. */
SEC("kprobe/__netif_receive_skb")
int BPF_KPROBE(handle_netif_receive_skb, struct sk_buff *skb)
{
    __u32 key = 0;
    __u16 proto;
    struct netobs_nic_ingress *ni;

    if (!skb)
        return 0;
    proto = BPF_CORE_READ(skb, protocol);
    if (proto != bpf_htons(NETOBS_ETH_P_IP) && proto != bpf_htons(NETOBS_ETH_P_IPV6))
        return 0;

    ni = bpf_map_lookup_elem(&nic_ingress, &key);
    if (!ni)
        return 0;
    ni->ts_nic = bpf_ktime_get_ns();
    ni->ts_l3  = 0;
    ni->skb    = (__u64)(unsigned long)skb;
    return 0;
}

/* #65 receive path stage 별 kprobe.
 *
 * tcp_v4_rcv 는 L3 entry 로 socket lookup 이전이라 sk 인자가 없지만, #141 부터 skb->sk (early demux
 * 결과) 로 sock 을 복원해 L3 진입 timestamp 를 socket_cookie 키로 stash 한다. #173 부터는 sk 와 무관 하게
 * capture_nic_l3 로 per-CPU nic_ingress slot 에 L3 진입 시각 (ts_l3) 을 stamp 해, sock 인자 가 보장 되는
 * tcp_v4_do_rcv 에서 NIC→L3 (RCV_NIC) stage 를 귀속 / emit 할 수 있게 한다 (RCV_L3 자체 의 별도 emit 은
 * 두지 않는다). tcp_v4_do_rcv 부터는 sock 인자가 있어 sk_cgrp_data 기반 cgroup_id 로 수신 Pod 를 정확히
 * 식별한다. tcp_recvmsg 는 process context 라 bpf_get_current_cgroup_id() 도 정답을 주지만 sock 경로로
 * 통일해 다른 stage 와 동일한 키 공간을 유지한다. */
SEC("kprobe/tcp_v4_rcv")
int BPF_KPROBE(handle_tcp_v4_rcv, struct sk_buff *skb)
{
    capture_nic_l3(skb);
    stash_recv_l3(skb);
    return 0;
}

/* #173 tcp_v4_do_rcv 는 sock 인자 가 보장 되는 동기 콜체인 의 demux 단계 다. NIC→L3 (RCV_NIC) 와
 * L3→demux (RCV_DEMUX) 두 stage 를 함께 emit 한다. RCV_NIC 은 per-CPU slot 의 ts_nic / ts_l3 상관 이
 * 성공 한 경우 에만 emit_rcv_event 내부 가드 로 emit 되고, 상관 실패 시 자연 skip 된다. */
SEC("kprobe/tcp_v4_do_rcv")
int BPF_KPROBE(handle_tcp_v4_do_rcv, struct sock *sk, struct sk_buff *skb)
{
    emit_rcv_event(sk, skb, NETOBS_STAGE_RCV_NIC);
    emit_rcv_event(sk, skb, NETOBS_STAGE_RCV_DEMUX);
    return 0;
}

/* #103 IPv6 TCP receive path entry. tcp_v4_rcv 와 동일 패턴 으로 #141 부터 skb->sk 로 L3 진입
 * timestamp 를 stash 하고, #173 부터 capture_nic_l3 로 per-CPU nic_ingress 의 L3 진입 시각 을 stamp
 * 한다. RCV_L3 stage 의 event emit 은 본 hook 에서 수행 하지 않으며, NIC→L3 stage 는 tcp_v6_do_rcv 가
 * emit_rcv_event 로 emit 한다. */
SEC("kprobe/tcp_v6_rcv")
int BPF_KPROBE(handle_tcp_v6_rcv, struct sk_buff *skb)
{
    capture_nic_l3(skb);
    stash_recv_l3(skb);
    return 0;
}

/* #103 IPv6 TCP demux. tcp_v4_do_rcv 와 동일 시그니처. emit_rcv_event 가 family 분기 로 IPv6 흐름 도
 * 자연 capture 한다. #173 NIC→L3 (RCV_NIC) 와 L3→demux (RCV_DEMUX) 두 stage 를 함께 emit 한다. */
SEC("kprobe/tcp_v6_do_rcv")
int BPF_KPROBE(handle_tcp_v6_do_rcv, struct sock *sk, struct sk_buff *skb)
{
    emit_rcv_event(sk, skb, NETOBS_STAGE_RCV_NIC);
    emit_rcv_event(sk, skb, NETOBS_STAGE_RCV_DEMUX);
    return 0;
}

/* #103 / #197 UDP 트래픽 추적 helper. udp_sendmsg / udpv6_sendmsg / udp_recvmsg / udpv6_recvmsg 4 hook
 * 의 공통 흐름 을 추출 한다. pod_bytes 볼륨 은 connected / unconnected 무관 하게 누적 해 DNS / QUIC 등
 * unconnected UDP 볼륨 이 통째 로 누락 되던 #103 의 connected-only 한계 를 보완 한다. 5-tuple flow 는
 * connected 는 sk peer (skc_daddr / skc_dport), unconnected TX 는 msghdr->msg_name (sendto 목적지) 을
 * 파싱 해 emit 한다. unconnected RX 의 소스 는 udp_recvmsg 진입 시점 에 msg_name 이 아직 비어 있고 skb
 * 파싱 이 필요 해 flow 는 미emit (볼륨 은 pod_bytes 로 계상). entry-only 누적 이라 TX 는 size 인자 정확,
 * RX 는 user buffer size 라 partial recv 시 과대 계상 가능 (docs/netobs/protocol-coverage.md 참조). */
static __always_inline void handle_udp_msg(struct sock *sk, struct msghdr *msg,
                                           size_t size, __u8 direction)
{
    __u64 cgroup_id;
    __u16 family_raw;
    __u8 family;
    __u8 saddr[NETOBS_ADDR_LEN] = {0};
    __u8 daddr[NETOBS_ADDR_LEN] = {0};
    __u16 sport = 0, dport = 0;
    __u8 sk_state;

    if (!sk || size == 0)
        return;

    cgroup_id = bpf_get_current_cgroup_id();
    if (!cgroup_id)
        return;

    family_raw = BPF_CORE_READ(sk, __sk_common.skc_family);
    if (family_raw == NETOBS_AF_INET)
        family = NETOBS_AF_INET;
    else if (family_raw == NETOBS_AF_INET6)
        family = NETOBS_AF_INET6;
    else
        return;

    /* #197 볼륨 은 connected / unconnected 구분 없이 누적. connected 경로 의 기존 동작 은 불변 이고
     * unconnected UDP 볼륨 이 추가 로 계상 된다 (순수 additive). */
    inc_pod_bytes(cgroup_id, direction, NETOBS_LAYER_L4, (__u64)size, 0);

    sk_state = BPF_CORE_READ(sk, __sk_common.skc_state);
    sport = BPF_CORE_READ(sk, __sk_common.skc_num);

    if (sk_state == TCP_ESTABLISHED) {
        /* connected UDP: 목적지 가 sk 에 고정 돼 skc_daddr / skc_dport 로 읽는다 (#103 기존 경로). */
        if (family == NETOBS_AF_INET) {
            __u32 v4_src = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
            __u32 v4_dst = BPF_CORE_READ(sk, __sk_common.skc_daddr);
            __builtin_memcpy(saddr, &v4_src, 4);
            __builtin_memcpy(daddr, &v4_dst, 4);
        } else {
            /* #103 BPF_CORE_READ_INTO 의 array dst 1 byte 복사 버그 회피. */
            bpf_core_read(saddr, sizeof(saddr), &sk->__sk_common.skc_v6_rcv_saddr);
            bpf_core_read(daddr, sizeof(daddr), &sk->__sk_common.skc_v6_daddr);
        }
        dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
    } else if (direction == NETOBS_DIR_EGRESS && msg) {
        /* #197 unconnected UDP TX: 목적지 가 sk 에 없고 sendto/sendmsg 의 msg_name 에 있다. syscall 계층
         * 이 user sockaddr 을 kernel sockaddr_storage 로 복사 (move_addr_to_kernel) 한 뒤 udp_sendmsg 를
         * 호출 하므로 msg_name 은 kernel 포인터 라 BPF_CORE_READ 로 읽는다. 소스 는 sk 의 bound 주소
         * (미bind 시 0.0.0.0) 와 ephemeral 포트. 소켓 family 에 맞는 sockaddr 타입 으로 파싱 하고
         * sa_family 불일치 / 미제공 (namelen 부족) 은 skip 한다. */
        void *name = BPF_CORE_READ(msg, msg_name);
        int namelen = BPF_CORE_READ(msg, msg_namelen);
        if (!name)
            return;
        if (family == NETOBS_AF_INET) {
            struct sockaddr_in *sin = name;
            if (namelen < (int)sizeof(struct sockaddr_in) ||
                BPF_CORE_READ(sin, sin_family) != NETOBS_AF_INET)
                return;
            __be32 v4_dst = BPF_CORE_READ(sin, sin_addr.s_addr);
            __u32 v4_src = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
            __builtin_memcpy(daddr, &v4_dst, 4);
            __builtin_memcpy(saddr, &v4_src, 4);
            dport = bpf_ntohs(BPF_CORE_READ(sin, sin_port));
        } else {
            struct sockaddr_in6 *sin6 = name;
            if (namelen < (int)sizeof(struct sockaddr_in6) ||
                BPF_CORE_READ(sin6, sin6_family) != NETOBS_AF_INET6)
                return;
            bpf_core_read(daddr, sizeof(daddr), &sin6->sin6_addr);
            bpf_core_read(saddr, sizeof(saddr), &sk->__sk_common.skc_v6_rcv_saddr);
            dport = bpf_ntohs(BPF_CORE_READ(sin6, sin6_port));
        }
    } else {
        /* #197 unconnected RX: 소스 가 skb 에만 있어 flow 미emit (볼륨 은 위 pod_bytes 로 계상). */
        return;
    }

    inc_flow_bytes(cgroup_id, family, saddr, daddr, sport, dport,
                   17 /* IPPROTO_UDP */, direction, (__u64)size);
}

SEC("kprobe/udp_sendmsg")
int BPF_KPROBE(handle_udp_sendmsg, struct sock *sk, struct msghdr *msg, size_t size)
{
    handle_udp_msg(sk, msg, size, NETOBS_DIR_EGRESS);
    return 0;
}

SEC("kprobe/udp_recvmsg")
int BPF_KPROBE(handle_udp_recvmsg, struct sock *sk, struct msghdr *msg, size_t size)
{
    handle_udp_msg(sk, msg, size, NETOBS_DIR_INGRESS);
    return 0;
}

SEC("kprobe/udpv6_sendmsg")
int BPF_KPROBE(handle_udpv6_sendmsg, struct sock *sk, struct msghdr *msg, size_t size)
{
    handle_udp_msg(sk, msg, size, NETOBS_DIR_EGRESS);
    return 0;
}

SEC("kprobe/udpv6_recvmsg")
int BPF_KPROBE(handle_udpv6_recvmsg, struct sock *sk, struct msghdr *msg, size_t size)
{
    handle_udp_msg(sk, msg, size, NETOBS_DIR_INGRESS);
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

/* #197 tcp_send_ack(struct sock *sk) 는 수신측 이 지연 ACK 타이머 만료 / quickack / OFO 등 으로
 * standalone ACK 를 송신 하는 지점 이다 (데이터 에 piggyback 되는 ACK 는 tcp_write_xmit 경로 라 여기 를
 * 타지 않는다). tcp_rcv_established 가 stash 한 첫 미-ACK 데이터 수신 시각 과의 차분 을 "수신측 ACK 대기"
 * latency 로 emit_rcv_event (ACK_WAIT 분기) 가 채택 한다. sk 인자 만 있고 skb 는 없어 RCV_APP 과 동일 하게
 * skb=NULL 로 호출 한다. */
SEC("kprobe/tcp_send_ack")
int BPF_KPROBE(handle_tcp_send_ack, struct sock *sk)
{
    emit_rcv_event(sk, NULL, NETOBS_STAGE_ACK_WAIT);
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

    /* #103 drop flow 5-tuple 의 IPv6 확장. fill_conn_from_sock 가 family 분기 로 saddr / daddr 의
     * [16]byte 통합 슬롯 을 채우고 emit_event 가 family 라벨 을 함께 노출 한다. NETOBS_AF_INET /
     * NETOBS_AF_INET6 외 family 는 5-tuple 라벨 의미 없 어 자연 skip. */
    family = BPF_CORE_READ(sk, __sk_common.skc_family);
    if (family != NETOBS_AF_INET && family != NETOBS_AF_INET6)
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
    emit_event(&s, NETOBS_STAGE_DROP, reason, 0, 0, stack_id, 0, 0);
    return 0;
}
