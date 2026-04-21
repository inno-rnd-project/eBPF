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
