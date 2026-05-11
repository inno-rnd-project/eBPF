# netobs drop reason 매핑

netobs 가 emit 하는 `netobs_drop_total` 과 `netobs_drop_events_labeled_total` 메트릭의 `reason` 코드와 `drop_reason` 이름, `drop_category` 분류의 의미를 정리한다. 매핑은 런타임에 호스트 kernel 의 tracepoint format 파일을 파싱해 동적으로 구성되므로 본 문서는 검증 환경 (kernel 6.8.0-60-generic) 기준 스냅샷이다.

## 메트릭 표면

netobs 는 drop 이벤트를 두 종류의 메트릭으로 emit 한다. 카디널리티가 다르므로 용도에 맞게 골라 쓴다.

| 메트릭 | 라벨 | 용도 |
|---|---|---|
| `netobs_drop_total` | `reason` (kernel 정수 코드) | 전체 drop 추세 추적. 카디널리티 최소. |
| `netobs_drop_events_labeled_total` | `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `drop_reason` (정규화된 이름), `drop_category` (분류) | 운영 진단. Pod attribution + 사람이 읽는 이름 + 카테고리. |

## 동적 매핑 메커니즘

netobs agent 는 시작 시 다음 우선순위로 kernel 의 reason 테이블을 로드한다.

1. `DROP_REASON_FORMAT_PATH` 환경변수 (override 경로) 가 가리키는 파일
2. `/sys/kernel/tracing/events/skb/kfree_skb/format`
3. `/sys/kernel/debug/tracing/events/skb/kfree_skb/format`

위 파일의 `{ <code>, "<name>" }` 패턴을 정규식으로 추출하고 `SKB_DROP_REASON_` 또는 `SKB_` 접두어를 제거해 정규화된 이름을 만든다. 로드 실패 시 `REASON_<code>` 형식의 fallback 이름을 사용한다. agent 로그에 다음 한 줄로 적재 결과가 남는다.

```
drop reason runtime map loaded from /sys/kernel/tracing/events/skb/kfree_skb/format (86 entries)
```

`/sys/kernel/tracing` 마운트는 kernel 의 tracefs 로 별도 마운트 없이 호스트에 항상 노출된다. agent 가 `hostPID` 를 사용하므로 컨테이너 안에서도 접근 가능하다.

## drop_category 분류

`drop_category` 는 정규화된 이름에 대한 부분 문자열 매칭 우선순위에 따라 결정된다. 운영자가 가장 자주 묻는 "어떤 종류의 drop 인가" 를 한 라벨로 답하기 위한 분류다. 매칭 순서가 결과를 결정한다 (앞 카테고리가 먼저 매칭되면 뒤 카테고리는 검사하지 않는다).

| 순서 | 카테고리 | 매칭 패턴 | 의미 |
|---|---|---|---|
| 1 | `socket` | `SOCKET` 포함 | 소켓 자체 또는 소켓 BPF 필터 / 버퍼 |
| 2 | `checksum` | `CSUM` 포함 | L3/L4 체크섬 실패 |
| 3 | `policy` | `NETFILTER`, `FILTER`, `TC_`, `XDP` 포함 | 네트워크 정책 (iptables / nftables / tc / XDP / cgroup BPF) |
| 4 | `queue` | `QDISC`, `QUEUE`, `BACKLOG`, `RING` 포함 | 큐 / 백로그 / NIC ring 포화 |
| 5 | `resource` | `NOMEM`, `MEM`, `FULL_RING` 포함 | 메모리 압박, 프로토콜 글로벌 메모리 한계 |
| 6 | `routing` | `ROUTE`, `NOROUTES`, `RPFILTER`, `NEIGH` 포함 | 라우팅 / RPF / 이웃 (ARP / NDISC) 해상 실패 |
| 7 | `protocol` | `PROTO`, `IP_`, `PKT_`, `HDR` 포함 | 프로토콜 헤더 / 패킷 크기 / 알 수 없는 L4 |
| 8 | `device` | `TAP`, `DEV_`, `OTHERHOST` 포함 | 디바이스 헤더 / TAP / 자신 앞이 아닌 패킷 |
| 9 | `unknown` | 위 어느 것도 매칭 안 됨 | 분류 보류 (TCP 상태 머신 관련 다수가 여기에 속함) |

## kernel 6.8.0-60-generic 의 reason 표

코드 2 부터 86 까지의 86 개 reason (코드 87 `MAX` 는 sentinel). `정규화된 이름` 열은 agent 가 emit 하는 `drop_reason` 라벨 값이다. `분류` 열은 위 표의 우선순위를 적용한 결과다.

| 코드 | 정규화된 이름 | 분류 | 한글 설명 |
|---:|---|---|---|
| 2 | `NOT_SPECIFIED` | unknown | 호출 측이 사유를 명시하지 않음 |
| 3 | `NO_SOCKET` | socket | 패킷에 매칭되는 소켓이 없음 |
| 4 | `PKT_TOO_SMALL` | protocol | 패킷이 최소 헤더 크기보다 작음 |
| 5 | `TCP_CSUM` | checksum | TCP 체크섬 실패 |
| 6 | `SOCKET_FILTER` | socket | 소켓에 부착된 BPF 필터가 drop 결정 |
| 7 | `UDP_CSUM` | checksum | UDP 체크섬 실패 |
| 8 | `NETFILTER_DROP` | policy | netfilter (iptables / nftables) drop |
| 9 | `OTHERHOST` | device | 자신 앞이 아닌 L2 주소 (promisc 환경) |
| 10 | `IP_CSUM` | checksum | IPv4 헤더 체크섬 실패 |
| 11 | `IP_INHDR` | protocol | IPv4 헤더 검증 실패 |
| 12 | `IP_RPFILTER` | policy | reverse path filter 거부 |
| 13 | `UNICAST_IN_L2_MULTICAST` | unknown | L2 멀티캐스트 프레임에 유니캐스트 IP |
| 14 | `XFRM_POLICY` | unknown | IPsec / XFRM 정책 거부 |
| 15 | `IP_NOPROTO` | protocol | 알 수 없는 L4 프로토콜 |
| 16 | `SOCKET_RCVBUFF` | socket | 소켓 수신 버퍼 한계 초과 |
| 17 | `PROTO_MEM` | resource | 프로토콜 전역 메모리 한계 |
| 18 | `TCP_AUTH_HDR` | protocol | TCP-AO/MD5 옵션 헤더 검증 실패 |
| 19 | `TCP_MD5NOTFOUND` | unknown | 기대된 TCP MD5 키 없음 |
| 20 | `TCP_MD5UNEXPECTED` | unknown | 예상치 못한 TCP MD5 옵션 |
| 21 | `TCP_MD5FAILURE` | unknown | TCP MD5 검증 실패 |
| 22 | `TCP_AONOTFOUND` | unknown | TCP-AO 키 없음 |
| 23 | `TCP_AOUNEXPECTED` | unknown | 예상치 못한 TCP-AO |
| 24 | `TCP_AOKEYNOTFOUND` | unknown | TCP-AO 매칭 키 없음 |
| 25 | `TCP_AOFAILURE` | unknown | TCP-AO 검증 실패 |
| 26 | `SOCKET_BACKLOG` | socket | 소켓 백로그 큐 포화 |
| 27 | `TCP_FLAGS` | unknown | TCP 플래그 조합 부적합 |
| 28 | `TCP_ZEROWINDOW` | unknown | TCP zero window 상태 |
| 29 | `TCP_OLD_DATA` | unknown | 이미 ACK 된 옛 TCP 데이터 |
| 30 | `TCP_OVERWINDOW` | unknown | TCP 수신 윈도우 초과 |
| 31 | `TCP_OFOMERGE` | unknown | TCP OFO 큐 병합 중 drop |
| 32 | `TCP_RFC7323_PAWS` | unknown | RFC 7323 PAWS 검증 실패 |
| 33 | `TCP_OLD_SEQUENCE` | unknown | 옛 TCP 시퀀스 |
| 34 | `TCP_INVALID_SEQUENCE` | unknown | 잘못된 TCP 시퀀스 |
| 35 | `TCP_RESET` | unknown | TCP RST 수신 |
| 36 | `TCP_INVALID_SYN` | unknown | 잘못된 SYN |
| 37 | `TCP_CLOSE` | unknown | 닫힌 소켓 대상 패킷 |
| 38 | `TCP_FASTOPEN` | unknown | TCP fastopen 실패 |
| 39 | `TCP_OLD_ACK` | unknown | 옛 TCP ACK |
| 40 | `TCP_TOO_OLD_ACK` | unknown | 너무 옛 TCP ACK |
| 41 | `TCP_ACK_UNSENT_DATA` | unknown | 보내지 않은 데이터에 대한 ACK |
| 42 | `TCP_OFO_QUEUE_PRUNE` | queue | TCP OFO 큐 정리로 drop |
| 43 | `TCP_OFO_DROP` | unknown | TCP OFO 큐 포화로 drop |
| 44 | `IP_OUTNOROUTES` | routing | 송신 경로 없음 |
| 45 | `BPF_CGROUP_EGRESS` | unknown | cgroup BPF 프로그램이 egress drop |
| 46 | `IPV6DISABLED` | unknown | IPv6 가 인터페이스에서 비활성 |
| 47 | `NEIGH_CREATEFAIL` | routing | 이웃 엔트리 생성 실패 |
| 48 | `NEIGH_FAILED` | routing | 이웃 해상 실패 (ARP / NDISC 시간초과) |
| 49 | `NEIGH_QUEUEFULL` | queue | 이웃 큐 포화 |
| 50 | `NEIGH_DEAD` | routing | 사망 상태 이웃 |
| 51 | `TC_EGRESS` | policy | tc egress qdisc/filter drop |
| 52 | `QDISC_DROP` | queue | qdisc 가 drop 결정 (예: bfifo 한계) |
| 53 | `CPU_BACKLOG` | queue | per-CPU 백로그 포화 |
| 54 | `XDP` | policy | XDP 프로그램이 drop 결정 |
| 55 | `TC_INGRESS` | policy | tc ingress qdisc/filter drop |
| 56 | `UNHANDLED_PROTO` | protocol | 처리되지 않은 L3 프로토콜 |
| 57 | `CSUM` | checksum | 일반 skb 체크섬 실패 |
| 58 | `GSO_SEG` | unknown | GSO 세그먼테이션 실패 |
| 59 | `UCOPY_FAULT` | unknown | userspace 복사 중 page fault |
| 60 | `DEV_HDR` | protocol | 디바이스 헤더 검증 실패 |
| 61 | `DEV_READY` | device | 디바이스 미준비 |
| 62 | `FULL_RING` | queue | NIC ring 포화 |
| 63 | `NOMEM` | resource | 할당 실패 |
| 64 | `HDR_TRUNC` | protocol | 헤더 잘림 |
| 65 | `TAP_FILTER` | policy | TAP 디바이스 필터 |
| 66 | `TAP_TXFILTER` | policy | TAP 디바이스 tx 필터 |
| 67 | `ICMP_CSUM` | checksum | ICMP 체크섬 실패 |
| 68 | `INVALID_PROTO` | protocol | 잘못된 프로토콜 필드 |
| 69 | `IP_INADDRERRORS` | protocol | 잘못된 IP 주소 헤더 |
| 70 | `IP_INNOROUTES` | routing | 수신 경로 없음 |
| 71 | `PKT_TOO_BIG` | protocol | 경로 MTU 초과 |
| 72 | `DUP_FRAG` | unknown | 중복 IP 단편 |
| 73 | `FRAG_REASM_TIMEOUT` | unknown | 단편 재조립 시간초과 |
| 74 | `FRAG_TOO_FAR` | unknown | 단편 간 간격 초과 |
| 75 | `TCP_MINTTL` | unknown | 최소 TTL 미만 |
| 76 | `IPV6_BAD_EXTHDR` | protocol | 잘못된 IPv6 확장 헤더 |
| 77 | `IPV6_NDISC_FRAG` | unknown | NDISC 단편 |
| 78 | `IPV6_NDISC_HOP_LIMIT` | unknown | NDISC hop limit 부적합 |
| 79 | `IPV6_NDISC_BAD_CODE` | unknown | 잘못된 NDISC 코드 |
| 80 | `IPV6_NDISC_BAD_OPTIONS` | unknown | 잘못된 NDISC 옵션 |
| 81 | `IPV6_NDISC_NS_OTHERHOST` | device | NDISC NS 가 다른 호스트 대상 |
| 82 | `QUEUE_PURGE` | queue | 큐 비우기 중 drop |
| 83 | `TC_COOKIE_ERROR` | policy | tc cookie 오류 |
| 84 | `PACKET_SOCK_ERROR` | unknown | AF_PACKET 소켓 오류 |
| 85 | `TC_CHAIN_NOTFOUND` | policy | tc chain 미발견 |
| 86 | `TC_RECLASSIFY_LOOP` | policy | tc 재분류 루프 한계 |

코드 1 은 kernel format 파일에 정의되지 않으나 BPF 경로에서 일부 호출 site 가 보고할 수 있다. 이 경우 agent 는 `REASON_1` fallback 이름을 부여하며 분류는 `unknown` 이 된다.

## PromQL 예시

운영에서 자주 쓰는 쿼리 패턴이다.

```promql
# 노드별 카테고리별 drop rate (5분 평균, 초당 이벤트)
sum by (node, drop_category) (rate(netobs_drop_events_labeled_total[5m]))

# 정책 위반 (NetworkPolicy / iptables / tc) drop 만 추적
sum by (src_namespace, src_workload) (
  rate(netobs_drop_events_labeled_total{drop_category="policy"}[5m])
)

# TCP 상태 머신 관련 drop (분류 unknown 중 이름이 TCP_ 로 시작) Top-N
topk(10, sum by (drop_reason, src_workload) (
  rate(netobs_drop_events_labeled_total{drop_reason=~"TCP_.*"}[5m])
))

# 큐 포화 (NIC ring / qdisc / CPU backlog) 워크로드별
sum by (src_workload) (
  rate(netobs_drop_events_labeled_total{drop_category="queue"}[5m])
)

# 특정 reason 코드의 워크로드 분포 (예: NETFILTER_DROP)
sum by (src_namespace, src_workload, traffic_scope) (
  rate(netobs_drop_events_labeled_total{drop_reason="NETFILTER_DROP"}[5m])
)
```

`netobs_drop_total` 은 `reason` 정수 라벨만 가지므로 카테고리 필터링이 불가하다. 카테고리 분석에는 항상 `netobs_drop_events_labeled_total` 을 사용한다.

## 한계와 운영 노트

본 표는 kernel 6.8.0-60-generic 의 `enum skb_drop_reason` 정의를 기준으로 작성됐다. kernel 버전이 다르면 코드와 이름의 매핑이 달라지므로 agent 로그의 `drop reason runtime map loaded ... (N entries)` 한 줄이 본 표보다 우선한다. 코드의 의미가 바뀌는 회귀는 드물지만 N 값이 본 문서의 86 과 크게 다르면 호스트 kernel 의 `/sys/kernel/tracing/events/skb/kfree_skb/format` 을 직접 확인해 표를 재작성한다.

`drop_category` 분류기는 부분 문자열 매칭 기반이라 새로운 reason 이름이 들어와도 동작하지만, 분류 정확도가 의도와 다를 수 있다. 예를 들어 `PACKET_SOCK_ERROR` 는 의미상 소켓 관련이지만 `SOCKET` 이 아닌 `SOCK` 만 포함해 현재 `unknown` 으로 분류된다. 이런 분류 갭이 운영에서 문제 되는 경우 `internal/netobs/drop/reasons.go` 의 `Category` 함수에 패턴을 추가한다.
