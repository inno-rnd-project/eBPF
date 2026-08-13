# netobs IPv6 와 UDP 트래픽 추적 운영 가이드

이슈 #103의 IPv6 와 UDP 추적 확장 운영 가이드다. 기존 IPv4 TCP 한정 가시성 부재를 해소하고 cluster 의 DNS/QUIC/SRT 같은 UDP 워크로드와 IPv6 트래픽을 동일 메트릭/dashboard/API 체계로 추적 가능하게 한다.

## 적용 대상과 비목표

- 적용 대상은 dev/staging/prod 모든 cluster의 netobs-agent. kernel 5+ (RTX 3090 dev `6.8.0-60-generic` 와 그 외 노드 `6.2.0-33-generic` 전제). IPv6/UDP probe는 모두 `attachOptionalKprobe` 등록이라 kernel 4.x 노드에서도 fail-close 없이 자연 skip된다
- 비목표는 SCTP 추적, ICMP/ICMPv6 추적, IPv6 link-local `fe80::/10`/multicast `ff00::/8`/loopback `::1` 추적 (BPF 단 필터로 자동 제외), UDP unconnected RX 5-tuple 소스 추적 (`udp_recvmsg` 진입 시점에 소스가 skb에만 있어 skb 파싱 필요, 별도 follow-up), UDP packet-level stateful tracking, drop reason enum의 IPv6/UDP specific reason 세분화 (기존 enum 그대로 가드만 해제)

## BPF probe 카탈로그

#103 신설 probe 6 종.

| Symbol | 역할 | 시그니처 |
|---|---|---|
| `tcp_v6_rcv` | IPv6 TCP receive entry (stub) | `int(struct sk_buff*)` |
| `tcp_v6_do_rcv` | IPv6 TCP demux (sock 기반 event emit) | `int(struct sock*, struct sk_buff*)` |
| `udp_sendmsg` | IPv4 UDP TX (connected sk peer + unconnected msg_name) | `int(struct sock*, struct msghdr*, size_t)` |
| `udp_recvmsg` | IPv4 UDP RX (connected sk peer, unconnected는 볼륨만) | `int(struct sock*, struct msghdr*, size_t, ...)` |
| `udpv6_sendmsg` | IPv6 UDP TX (connected sk peer + unconnected msg_name) | `int(struct sock*, struct msghdr*, size_t)` |
| `udpv6_recvmsg` | IPv6 UDP RX (connected sk peer, unconnected는 볼륨만) | `int(struct sock*, struct msghdr*, size_t, ...)` |

기존 `tcp_v4_rcv`/`tcp_v4_do_rcv`/`tcp_rcv_established`/`tcp_recvmsg` 4 종은 family 무관 단일 함수 (`tcp_rcv_established`와 `tcp_recvmsg`) 와 IPv4 전용 함수 (`tcp_v4_rcv`/`tcp_v4_do_rcv`) 의 조합 으로 동작 한다. `emit_rcv_event` 가 family 분기 처리 라 IPv4/IPv6 양쪽 event 가 동일 hook 으로 capture 된다.

## ip_version 라벨 PromQL join 패턴

`netobs_flow_bytes_total` 과 `netobs_drop_events_flow_total` 에 `ip_version` 라벨 추가 (값 `"4"` 또는 `"6"`).

IPv4 와 IPv6 합산 (기존 PromQL 그대로):

```promql
sum by (src_namespace, src_pod) (rate(netobs_flow_bytes_total[5m]))
```

`sum by` 절이 `ip_version` 라벨을 자연 흡수해 IPv4/IPv6 합산으로 동작.

IPv4/IPv6 분리 (dashboard panel 200 패턴):

```promql
sum by (ip_version) (rate(netobs_flow_bytes_total{src_namespace="my-ns"}[5m]))
```

IPv6 만 (DNS over IPv6 모니터링 예):

```promql
sum by (src_namespace, src_pod) (rate(netobs_flow_bytes_total{ip_version="6",protocol="udp"}[5m]))
```

## 비-routable IPv6 자동 필터

`inc_flow_bytes` 진입 시점에 다음 3 종 IPv6 prefix 가 자동 skip 된다 (BPF 단 zero-cost):

- link-local `fe80::/10` (`addr[0] == 0xfe && (addr[1] & 0xc0) == 0x80`)
- multicast `ff00::/8` (`addr[0] == 0xff`)
- loopback `::1` (`addr[0..14] == 0 && addr[15] == 0x01`)

CoreDNS link-local 트래픽 과 IPv6 Router Advertisement 같은 cluster 자체 신호 가 flow_bytes 의 cardinality 를 폭증 시키지 않게 한다. IPv4 의 동등 필터 (`127.0.0.0/8`, `224.0.0.0/4`, `169.254.0.0/16`) 는 별도 follow-up.

## UDP cardinality 추정과 limitation

UDP 볼륨 (`netobs_pod_bytes_total`) 은 connected / unconnected 무관 하게 계상 한다 (#197). 5-tuple flow (`netobs_flow_bytes_total`) 는 connected 는 sk peer (skc_daddr / skc_dport), unconnected TX 는 `msghdr->msg_name` (sendto 목적지, syscall 이 kernel sockaddr_storage 로 복사 한 뒤라 `BPF_CORE_READ` 로 파싱) 로 emit 한다. unconnected RX 의 소스 는 `udp_recvmsg` 진입 시점 에 msg_name 이 비어 있고 skb 파싱 이 필요 해 flow 는 미emit 이고 볼륨 만 계상 한다 (follow-up).

cardinality 추정.

- IPv4 TCP 기존 cardinality (활성 5-tuple) 의 약 1.5-2 배 증가 예상
- IPv6 트래픽 가 IPv4 의 약 0.3 배 (cluster internal 의 대다수 가 IPv4 라 dev/prod 모두 적용)
- UDP connected 는 보통 DNS resolver socket (한 Pod 당 1-2 개) 와 QUIC connection (server 당 수 십 개) 라 노드 당 100-300 entries 추가

BPF LRU map `flow_bytes` 의 `max_entries=1024` 가 자연 evict 정책 으로 cardinality 폭발 차단. 운영 모니터링 (`netobs_bpf_map_utilization_ratio{map="flow_bytes"}`) 가 0.8 초과 시 sampling 또는 map size 증대 follow-up 결정.

RX 정확도: #443부터 `udp_recvmsg`/`udpv6_recvmsg` kretprobe의 ret(실제 user로 복사된 byte)로 누적한다. 종전 entry size 인자는 user buffer 크기라 CoreDNS처럼 큰 버퍼(65535)로 작은 응답을 받는 워크로드에서 수백 배 과대 계상됐고, 이 결함이 제거됐다. TX는 datagram 전송이 all-or-nothing이라 entry size 누적을 유지한다(성공 시 ret와 동일).

## self-filter IPv6 확장

netobs-agent 자체의 Prometheus scrape 와 Kubernetes informer 트래픽 은 기존 self-filter (Pod IP 매칭) 로 제외. `IPToString` helper 가 family 분기 로 IPv4/IPv6 표현 을 자동 반환 하므로 별도 self-filter 코드 변경 없이 IPv6 self-traffic 도 자연 제외 된다. `NETOBS_FLOW_ALLOW_NAMESPACES` 환경 변수 는 IP version 무관 하게 namespace 기준 으로만 동작.

## kernel 호환성 요건

- 최소 kernel: 5.8+ (event 전달이 BPF ringbuf 라 `BPF_MAP_TYPE_RINGBUF` 도입 커널이 하한. BTF CO-RE 와 `tcp_v6_rcv`/`udp_sendmsg` 안정 심볼 전제 포함, 기능별 하한은 docs/deploy/kernel-matrix.md 참조)
- 검증 cluster kernel: gpu 노드 `6.8.0-60-generic`, 그 외 `6.2.0-33-generic`
- 호환성 검사: `kubectl debug node/<name> --image=alpine:3.18 -ti -- chroot /host grep -E ' (tcp_v6_rcv|udp_sendmsg|udpv6_sendmsg)$' /proc/kallsyms` 로 신규 6 심볼 존재 확인. 없으면 해당 kprobe attach 가 fail 하고 `netobs_bpf_program_loaded{symbol="..."} == 0` 로 표시

## dashboard panel 가이드

`netobs.json` 의 신규 panel 2 종.

- panel id 200: `Network flow bytes by ip_version (IPv4/IPv6 stacked)`. `sum by(ip_version)(rate(netobs_flow_bytes_total[5m]))` query. ip_version=4 blue, ip_version=6 red 색상. flow_bytes 는 `NETOBS_FLOW_ALLOW_NAMESPACES` 가 설정 된 namespace 만 emit 라 빈 panel 일 수 있음
- panel id 201: `Drop events by ip_version (5m rate)`. `drop_category` 별 분리

## Troubleshooting

### `netobs_flow_bytes_total{ip_version="6"}` 시리즈 부재

다음 순서로 점검.

1. `netobs_bpf_program_loaded{symbol="tcp_v6_rcv"} == 1` 확인. 0 이면 kernel 의 `tcp_v6_rcv` 심볼 부재 (`grep tcp_v6_rcv /proc/kallsyms` 로 확인) 또는 attach 권한 부재
2. `NETOBS_FLOW_ALLOW_NAMESPACES` 환경 변수 에 대상 namespace 설정. 비어 있으면 flow_bytes 자체 가 emit 안 됨 (cardinality 보호)
3. 실제 IPv6 트래픽 활성 여부 확인. `kubectl exec` 로 `ss -tn6` 또는 `ip -6 route` 로 cluster 의 IPv6 connectivity 검증
4. link-local/multicast/loopback 만 발생 했는지 확인. 이 3 종은 BPF 단 필터 로 자동 skip 되어 flow_bytes 에 등장 하지 않음

### `udp_sendmsg` attach 후 `protocol="udp"` 시리즈 부재

`netobs_bpf_program_loaded{symbol="udp_sendmsg"} == 1` 이지만 `netobs_flow_bytes_total{protocol="udp"}` 가 비어 있는 경우.

1. handle_udp_msg 의 sk_state == TCP_ESTABLISHED (connected UDP) 가드 미통과. 대부분의 DNS resolver 는 unconnected sendto 라 본 PR 범위 외 (별도 follow-up)
2. `kubectl exec <dns-pod> -- ss -un4 -p` 로 connected UDP socket 존재 여부 확인

### Pod 의 IPv6 트래픽 이 `ip_version="4"` 로 분류 됨

`IPToString` helper 의 family 분기 가 IPv4 로 잘못 판정 된 경우. 가능성.

1. dual-stack Pod 의 socket 이 IPv4-mapped IPv6 (`::ffff:1.2.3.4`) 형식. `skc_family` 는 AF_INET6 (10) 반환 이라 정상 IPv6 분류 되어야 함. log 또는 raw event capture 로 family 값 검증
2. eBPF struct layout 불일치. `go test -run TestFlowKeySize ./internal/netobs/ebpf/` 로 size 회귀 가드 통과 여부 확인
