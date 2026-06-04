#ifndef __NETOBS_COMMON_H__
#define __NETOBS_COMMON_H__

#define NETOBS_COMM_LEN 16

/* AF_INET 은 linux/socket.h 의 socket family 상수다. BPF 환경에서 vmlinux.h 는 enum 만 dump 하므로
 * 본 매크로를 명시 정의해 #64 의 drop flow IPv4 가드 (sk_family == AF_INET) 에서 hardcoded 2 보다
 * 가독성을 확보한다. */
#define NETOBS_AF_INET 2

/* #103 IPv6 트래픽 추적 확장 의 family 식별자. linux/socket.h 의 AF_INET6 상수 (10) 와 동일.
 * fill_conn_from_sock 가 sk_family 검사 후 본 매크로로 분기 하며 inc_flow_bytes 의 가드 도 family
 * enum 검사 로 IPv4 와 IPv6 양쪽 을 허용 한다. */
#define NETOBS_AF_INET6 10

/* #103 IPv6 주소 length (byte). v4/v6 통합 표현 시 [16]byte 슬롯 으로 통일 한다. IPv4 는 첫 4 byte
 * 만 사용 하며 나머지 12 byte 는 0 으로 채워 진다 (BPF 측 __builtin_memset 로 초기화). */
#define NETOBS_ADDR_LEN 16

enum netobs_event_stage {
    NETOBS_STAGE_SENDMSG_RET    = 1,
    NETOBS_STAGE_TO_VETH        = 2,
    NETOBS_STAGE_TO_DEVQ        = 3,
    NETOBS_STAGE_RETRANS        = 4,
    NETOBS_STAGE_DROP           = 5,
    /* #65 receive path stage 4 종. tcp_v4_rcv 진입 시점은 sk 가 아직 socket lookup 전이라 Pod
     * 귀속이 불가능해 emit 을 보류한다 (kprobe 만 부착 유지). 나머지 3 단계는 sock 인자가 있어
     * sk_cgrp_data 기반 cgroup_id 로 Pod 귀속이 가능하다. */
    NETOBS_STAGE_RCV_L3         = 6,
    NETOBS_STAGE_RCV_DEMUX      = 7,
    NETOBS_STAGE_RCV_ESTABLISHED = 8,
    NETOBS_STAGE_RCV_APP        = 9,
    /* #82 send path stage 분해. tcp_sendmsg (= STAGE_SENDMSG_RET) 와 __dev_queue_xmit
     * (= STAGE_TO_DEVQ) 사이의 두 stage 를 추가해 send path 의 kernel 내부 처리 시간을 4 분
     * 단위로 노출한다. tcp_write_xmit 는 TCP window throttle / nagle / cwnd 등 control path
     * latency 를 cover 하고, __tcp_transmit_skb 는 개별 segment transmit 시점이라 TSO/GSO
     * 활성 시 첫 segment 만 측정한다 (seen_transmit flag). */
    NETOBS_STAGE_TCP_WRITE_XMIT  = 10,
    NETOBS_STAGE_TCP_TRANSMIT_SKB = 11,
};

/* pod_bytes 누적 맵의 key/value. key는 (cgroup_id, direction, layer) 삼중 합성이며 동일 Pod의
 * (egress, l4) / (egress, nic) / (ingress, l4) 세 조합에 대해 각각 별도 카운터 슬롯이 생긴다.
 * direction은 enum netobs_byte_direction, layer는 enum netobs_byte_layer를 따른다. value의
 * bytes는 누적 바이트 (TX는 syscall payload 또는 skb->len, RX는 사용자공간이 읽어간 bytes),
 * packets는 누적 패킷 수다. 본 맵은 LRU PERCPU HASH로 운용해 종료 Pod의 stale entry는 자연
 * evict되고 핫 패스에서 CPU 간 contention이 없도록 한다. */
enum netobs_byte_direction {
    NETOBS_DIR_EGRESS  = 0,
    NETOBS_DIR_INGRESS = 1,
};

enum netobs_byte_layer {
    NETOBS_LAYER_NIC = 0,
    NETOBS_LAYER_L4  = 1,
};

struct netobs_pod_bytes_key {
    __u64 cgroup_id;
    __u8  direction;
    __u8  layer;
    __u8  pad[6];
};

struct netobs_pod_bytes_value {
    __u64 bytes;
    __u64 packets;
};

/* #85 Pod 간 정상 flow 의 5-tuple RX/TX 추적 map 의 key/value. cgroup_id 와 5-tuple 양쪽을 모두 보관해
 * 동일 connection 의 양 종단 Pod 가 서로 다른 entry 로 누적 되도록 한다. direction 을 key 에 포함해
 * 동일 connection 의 egress 와 ingress 도 별도 entry 로 누적 된다. saddr / daddr 은 network byte
 * order 로 fill_conn_from_sock 의 변환 규칙과 정합 한다.
 *
 * #103 IPv6 와 UDP 추적 확장 으로 saddr / daddr 을 [16]byte 통합 슬롯 (NETOBS_ADDR_LEN) 으로 변경
 * 했다. IPv4 는 첫 4 byte 만 사용 하며 나머지 12 byte 는 0 으로 초기화 된다. family 필드 가 추가 되어
 * userspace 가 ip_version 라벨 산정 (NETOBS_AF_INET → "4", NETOBS_AF_INET6 → "6") 에 사용 한다. */
struct netobs_flow_key {
    __u64 cgroup_id;
    __u8  saddr[NETOBS_ADDR_LEN];   /* network byte order, IPv4 는 첫 4 byte */
    __u8  daddr[NETOBS_ADDR_LEN];   /* network byte order, IPv4 는 첫 4 byte */
    __u16 sport;            /* host byte order */
    __u16 dport;            /* host byte order */
    __u8  protocol;         /* IP protocol number (6=TCP, 17=UDP) */
    __u8  direction;        /* netobs_byte_direction */
    __u8  family;           /* NETOBS_AF_INET (2) 또는 NETOBS_AF_INET6 (10) */
    __u8  pad;
};

struct netobs_flow_value {
    __u64 bytes;
};

struct netobs_start_info {
    __u64 ts_ns;
    __u64 cgroup_id;
    __u64 socket_cookie;    /* sock->sk_cookie */

    __u8  saddr[NETOBS_ADDR_LEN];   /* network byte order, IPv4 는 첫 4 byte */
    __u8  daddr[NETOBS_ADDR_LEN];   /* network byte order, IPv4 는 첫 4 byte */
    __u32 pid;
    __u32 tid;

    __u32 ifindex;          /* skb->dev->ifindex or sk_bound_dev_if */
    __u32 skb_iif;          /* skb ingress ifindex */

    __u16 sport;            /* host byte order */
    __u16 dport;            /* host byte order */

    char  comm[NETOBS_COMM_LEN];

    __u8  seen_veth;
    __u8  seen_devq;
    __u8  ret_seen;
    __u8  protocol;         /* IP protocol number (IPPROTO_TCP=6, IPPROTO_UDP=17). #64 의 drop flow
                              5-tuple emit 에 사용. */

    /* #65 TCP 상태 메트릭. emit_rcv_event 가 fill_tcp_state 로 채우며 다른 caller 는 0 으로 둔다.
     * srtt_us 는 kernel 의 << 3 scale 을 BPF 에서 >> 3 처리해 실제 µs 단위로 저장한다. */
    __u32 snd_cwnd;
    __u32 srtt_us;
    __u32 snd_ssthresh;

    /* snd_ssthresh (u32) 뒤 ts_write_xmit (u64) 의 8-byte align 으로 컴파일러가 자동 4-byte
     * padding 을 삽입한다. Go 측 generated NetObsNetobsStartInfo 에는 명시적 _ [4]byte 슬롯이
     * 들어가 있으므로 C 측에도 본 슬롯을 명시 선언해 컴파일러 의존성 없이 layout 일관성을
     * 보장한다.
     */
    __u8  pad_align[4];

    /* #82 send path stage 분해. tcp_write_xmit / __tcp_transmit_skb 의 entry timestamp 를
     * carry-over 해 kretprobe 시점에서 latency 산정에 사용한다. seen_transmit 는 TSO/GSO 활성
     * 시 단일 sendmsg 가 N 회의 __tcp_transmit_skb 를 트리거할 때 첫 segment 만 latency 를 측정
     * 하도록 가드해 starts map slot race 를 회피한다. */
    __u64 ts_write_xmit;
    __u64 ts_transmit_skb;
    __u8  seen_write_xmit;
    __u8  seen_transmit;
    /* #103 family enum (NETOBS_AF_INET / NETOBS_AF_INET6). fill_conn_from_sock 에서 sk_family 를
     * 한 번 읽 어 본 슬롯에 stash 한다. tcp_sendmsg_ret 의 inc_flow_bytes 호출 시 sk 가 직접 인자
     * 로 노출 되지 않 으므로 본 family enum 으로 IPv4/IPv6 가드 를 수행 한다. 기존 is_ipv4 flag 의
     * 의미 확장 형태 다. */
    __u8  family;
    __u8  pad82[5];
};

struct netobs_event {
    __u64 ts_ns;
    __u64 cgroup_id;
    __u64 socket_cookie;    /* sock->sk_cookie */

    __u8  saddr[NETOBS_ADDR_LEN];   /* network byte order, IPv4 는 첫 4 byte */
    __u8  daddr[NETOBS_ADDR_LEN];   /* network byte order, IPv4 는 첫 4 byte */
    __u32 pid;
    __u32 tid;
    __u32 ret;
    __u32 latency_us;
    __u32 reason;

    __u32 ifindex;          /* skb->dev->ifindex or sk_bound_dev_if */
    __u32 skb_iif;          /* skb ingress ifindex */

    __u16 sport;
    __u16 dport;

    char  comm[NETOBS_COMM_LEN];

    __u8  stage;
    __u8  protocol;         /* IP protocol number (IPPROTO_TCP=6, IPPROTO_UDP=17). */
    __u8  family;           /* #103 NETOBS_AF_INET / NETOBS_AF_INET6. userspace 가 ip_version 라벨
                              산정 에 사용. */
    __u8  pad;

    /* #65 TCP 상태 메트릭. rcv_* stage 의 emit 에서만 채워지며 그 외 stage 는 0. srtt_us 는 kernel
     * scale 을 BPF 에서 >> 3 한 실제 µs 단위다. 본 3 필드 추가로 struct size 가 88 byte → 96 byte 로
     * 확장되며 (12 byte 필드 + 8-align trailing 의 정합) Go 측 Event 의 unsafe.Sizeof 회귀 가드도
     * 96 으로 함께 갱신한다. #83 에서 stack_id 와 pad83 추가로 96 → 104 byte 로 재확장된다. */
    __u32 snd_cwnd;
    __u32 srtt_us;
    __u32 snd_ssthresh;

    /* #83 drop event 의 kernel stack capture. handle_kfree_skb_reason 에서 bpf_get_stackid 의
     * 반환값을 그대로 carry 하며 drop 외 stage 는 -1 로 명시 가드된다. __s32 4 byte 뒤에 컴파일러가
     * 8-byte align trailing padding 4 byte 를 자동 삽입하나 #82 의 pad82 와 동일 패턴으로 pad83 을
     * 명시 선언해 컴파일러 의존성 없이 C / Go layout 일관성을 확보한다. struct size 는 96 → 104
     * byte 로 확장된다. */
    __s32 stack_id;
    __u8  pad83[4];
};

#endif
