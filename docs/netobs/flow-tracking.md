# Pod 간 정상 flow 의 5-tuple RX/TX 실시간 추적

## 배경

기존 `netobs_drop_events_flow_total`은 drop된 flow의 5-tuple만 노출한다. 정상 flow의 RX/TX 대역폭은 `netobs_pod_bytes_total{cgroup_id, direction, layer}`로 Pod 단위 합계만 노출되며 connection 단위 추적이 부재하다. 본 PR은 #85의 5-tuple 단위 정상 flow 추적 layer를 도입해 Pod 간 네트워크 흐름의 connection-level 가시성을 확보한다.

## 4 stage 개요

| stage | 위치 | 책임 |
|---|---|---|
| BPF flow_bytes map | `bpf/netlat.bpf.c` | LRU_HASH 1024 cap의 5-tuple → bytes 누적 |
| inc_flow_bytes hook | `bpf/netlat.bpf.c` | `tcp_sendmsg_ret` egress와 `tcp_cleanup_rbuf` ingress의 5-tuple 누적 |
| userspace collector | `internal/netobs/flow/` | scrape 시점 BPF map iterate와 `netobs_flow_bytes_total` emit |
| cardinality 가드 | `internal/netobs/metrics/flowguard.go` | namespace allow-list + LRU sampling으로 series 폭주 차단 |

## BPF struct 확장

`bpf/common.h`에 신규 `netobs_flow_key` struct를 추가한다. 5-tuple (saddr, daddr, sport, dport, protocol) 과 direction을 key에 포함해 동일 connection의 egress와 ingress가 별도 entry로 누적된다. 8-byte align trailing padding 회피를 위해 #82와 #83의 명시 pad 패턴을 동일 적용한다. `netobs_flow_value`는 bytes 누적 1 필드만 둔다.

## BPF map 선택

`BPF_MAP_TYPE_LRU_HASH` (max_entries=1024) 를 채택한다. `pod_bytes`의 LRU_PERCPU_HASH와 분리해 per-CPU × 1024 entry의 memory footprint 부담을 회피한다. race 안전성은 helper 내부의 `__sync_fetch_and_add`로 확보한다.

## hook 누적

`tcp_sendmsg_ret`의 ret > 0 분기와 `tcp_cleanup_rbuf`의 copied > 0 분기에서 `inc_flow_bytes`를 호출한다. 두 helper는 IPv4 한정 (`sk_family == NETOBS_AF_INET`) 가드와 cgroup_id 0 skip 가드를 둬 host 작업과 비-IPv4 flow를 자동 제외한다.

## userspace collector

`internal/netobs/flow/collector.go` 신규 패키지에 `flow.Collector`를 둔다. `internal/netobs/podbytes/Collector`와 동일 패턴 (`atomic.Pointer` 기반 SetMap, BPF map iterate, throttled error log) 으로 구현한다. scrape 시점에 BPF map의 모든 entry를 iterate하고 FlowGuard.Admit 통과 entry만 `netobs_flow_bytes_total`로 emit한다.

### 신규 메트릭

```
netobs_flow_bytes_total{node, src_namespace, src_workload, src_pod, src_pod_uid, src_ip, src_port, dst_namespace, dst_pod_uid, dst_ip, dst_port, protocol, direction}
```

이슈 본문의 8 라벨에 `src_pod_uid`, `src_workload`, `node` 3 라벨을 추가해 기존 pod-level 메트릭과 라벨 셋 정합을 유지한다. dst 측은 `dst_namespace`와 `dst_pod_uid`를 `dstClassifier` 패턴으로 선택 노출한다.

## cardinality 가드

`internal/netobs/metrics/flowguard.go` 신설로 `FlowGuard` struct를 도입한다. `DropFlowGuard` 패턴 (namespace allow-list + LRU sampling) 을 차용하나 정상 flow와 drop flow의 admit 결과가 독립이라 별도 max_active로 cap한다.

### opt-in 정책

`NETOBS_FLOW_ALLOW_NAMESPACES` env가 비어 있으면 `flow.Collector`가 BPF map iterate 자체를 skip한다. BPF map의 LRU eviction 외에 userspace의 scrape 비용이 0으로 유지된다. `NETOBS_FLOW_MAX_ACTIVE` env로 LRU cap을 override 가능 (default 1024).

## self-health 연계

`internal/netobs/selfhealth/refresher.go`의 `netobs_bpf_map_utilization_ratio`에 `map="flow_bytes"` 라벨을 추가해 1024 LRU cap의 포화 신호를 노출한다.

## src / dst 라벨의 sender convention

본 메트릭의 `src_*` 라벨은 항상 송신측 Pod, `dst_*` 라벨은 항상 수신측 Pod를 가리킨다. BPF가 sk에서 채운 raw 5-tuple은 `src=local` 형태이나 ingress 방향에서는 `src`와 `dst`를 swap해 프로젝트 내 `emit_rcv_event` 의 saddr/daddr swap 패턴 및 `netobs_stage_events_labeled_total` / `netobs_drop_events_flow_total` 의 sender 의미 라벨과 정합을 맞춘다.

- egress (BPF cgroup 주인이 sender): `src_*` = local Pod, `dst_*` = remote peer
- ingress (BPF cgroup 주인이 receiver): swap 적용으로 `src_*` = remote peer (IP resolve), `dst_*` = local Pod

FlowGuard의 namespace 가드는 swap과 무관하게 본 노드의 local Pod namespace로 검사한다. 운영자가 `NETOBS_FLOW_ALLOW_NAMESPACES`에 등록한 namespace의 Pod이 보내거나 받는 모든 flow의 양 방향이 본 노드 agent에서 captured 된다.

## 양 종단 동시 관측

동일 connection은 Pod A의 agent와 Pod B의 agent 양쪽에서 각각 관측된다. 한 connection에 대해 BPF flow_bytes map에는 다음 4 entry가 분산 누적된다.

- Pod A의 agent: `(cgroup_A, egress)` 와 `(cgroup_A, ingress)` 두 entry
- Pod B의 agent: `(cgroup_B, egress)` 와 `(cgroup_B, ingress)` 두 entry

cgroup_id가 노드별로 다르므로 BPF 측에서는 자동으로 별개 entry로 분리되나 sender convention 적용 후 같은 connection의 동일 방향 시리즈가 양 agent에서 동일 라벨 셋으로 보고된다 (예: A의 egress = B의 ingress, label 동일). 운영자가 cluster-wide 합산 시 직접 중복 계산되므로 한 방향만 채택해 중복 회피한다.

```
sum by(...) (netobs_flow_bytes_total{direction="egress"})
```

본 한 방향 합산이 cluster의 모든 TCP 송신 바이트 (= 수신 바이트) 와 같다.

## sanity check

운영자는 다음 식으로 누락 신호를 검증한다. egress와 ingress 각각을 local Pod 기준으로 비교한다.

- egress: `sum by(src_pod_uid) (netobs_flow_bytes_total{direction="egress"})` 가 `netobs_pod_bytes_total{layer="l4", direction="egress"}` 와 ±5% 오차 범위에서 일치해야 한다
- ingress: `sum by(dst_pod_uid) (netobs_flow_bytes_total{direction="ingress"})` 가 `netobs_pod_bytes_total{layer="l4", direction="ingress"}` 와 ±5% 오차 범위에서 일치해야 한다

```
sum by(src_namespace, src_pod) (netobs_flow_bytes_total)
≈ sum by(src_namespace, src_pod) (netobs_pod_bytes_total{layer="l4"})
```

차이가 5% 초과로 벌어지면 BPF LRU eviction이 빈번해 flow 일부가 누락된 신호다. `netobs_bpf_map_utilization_ratio{map="flow_bytes"}`도 0.9 이상으로 동반 상승할 것이다.

## 비목표

- UDP / SCTP 등 non-TCP protocol은 본 PR 범위 밖
- IPv6 flow tracking은 본 PR 범위 밖 (`NETOBS_AF_INET` 가드와 정합)
- flow lifetime (connection start / end timestamp) 는 본 PR 범위 밖
- dashboard 패널은 #87의 cluster-node-pod drill-down 범위로 위임
- rca-summarizer의 `/snapshot` endpoint에 flow 노출은 RCA 측 follow-up

## 회귀 검증

dev cluster의 `observability-test` namespace의 client → server 자연 트래픽을 활용한다. `NETOBS_FLOW_ALLOW_NAMESPACES=observability-test` env 주입 후 다음 두 조건의 회귀 가드를 둔다.

- `count(netobs_flow_bytes_total{src_namespace="observability-test"}) >= 2` (egress + ingress 최소 2 entry)
- `sum(netobs_flow_bytes_total{src_workload="client"}) > 0` (Deployment hash 가 변동 하는 src_pod 대신 안정적인 src_workload 라벨 사용)

가드 통과 직후 임시 env 제거로 dev cluster 환경을 PR 시작 전 상태로 복구한다.
