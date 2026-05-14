#ifndef __NETOBS_COMMON_H__
#define __NETOBS_COMMON_H__

#define NETOBS_COMM_LEN 16

enum netobs_event_stage {
    NETOBS_STAGE_SENDMSG_RET = 1,
    NETOBS_STAGE_TO_VETH     = 2,
    NETOBS_STAGE_TO_DEVQ     = 3,
    NETOBS_STAGE_RETRANS     = 4,
    NETOBS_STAGE_DROP        = 5,
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
    __u8  pad0;
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
    __u8  pad[3];
};

#endif
