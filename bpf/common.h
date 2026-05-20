#ifndef __NETOBS_COMMON_H__
#define __NETOBS_COMMON_H__

#define NETOBS_COMM_LEN 16

/* AF_INET 은 linux/socket.h 의 socket family 상수다. BPF 환경에서 vmlinux.h 는 enum 만 dump 하므로
 * 본 매크로를 명시 정의해 #64 의 drop flow IPv4 가드 (sk_family == AF_INET) 에서 hardcoded 2 보다
 * 가독성을 확보한다. */
#define NETOBS_AF_INET 2

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

struct netobs_start_info {
    __u64 ts_ns;
    __u64 cgroup_id;
    __u64 socket_cookie;    /* sock->sk_cookie */

    __u32 saddr;            /* network byte order */
    __u32 daddr;            /* network byte order */
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
                              5-tuple emit 에 사용. 기존 pad0 슬롯을 reuse 해 본 필드 추가만으로는
                              struct size 가 변하지 않는다 (#65 의 snd_cwnd / srtt_us / snd_ssthresh
                              추가로 struct 전체는 별도로 확장됨). */

    /* #65 TCP 상태 메트릭. emit_rcv_event 가 fill_tcp_state 로 채우며 다른 caller 는 0 으로 둔다.
     * srtt_us 는 kernel 의 << 3 scale 을 BPF 에서 >> 3 처리해 실제 µs 단위로 저장한다. */
    __u32 snd_cwnd;
    __u32 srtt_us;
    __u32 snd_ssthresh;
};

struct netobs_event {
    __u64 ts_ns;
    __u64 cgroup_id;
    __u64 socket_cookie;    /* sock->sk_cookie */

    __u32 saddr;            /* network byte order */
    __u32 daddr;            /* network byte order */
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
    __u8  protocol;         /* IP protocol number (IPPROTO_TCP=6, IPPROTO_UDP=17). #64 의 drop flow
                              5-tuple emit 에 사용. 기존 pad[0] 슬롯을 reuse 해 본 필드 추가만으로는
                              struct size 가 변하지 않는다 (#65 의 TCP 상태 필드로 struct 전체는
                              별도로 확장됨). */
    __u8  pad[2];

    /* #65 TCP 상태 메트릭. rcv_* stage 의 emit 에서만 채워지며 그 외 stage 는 0. srtt_us 는 kernel
     * scale 을 BPF 에서 >> 3 한 실제 µs 단위다. 본 3 필드 추가로 struct size 가 88 byte → 96 byte 로
     * 확장되며 (12 byte 필드 + 8-align trailing 의 정합) Go 측 Event 의 unsafe.Sizeof 회귀 가드도
     * 96 으로 함께 갱신한다. */
    __u32 snd_cwnd;
    __u32 srtt_us;
    __u32 snd_ssthresh;
};

#endif
