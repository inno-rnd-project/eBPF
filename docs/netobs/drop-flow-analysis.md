# drop flow 5-tuple 분석 가이드

`netobs_drop_events_flow_total` 메트릭과 `NetObsDropBurst` alert로 packet drop의 정확한 5-tuple flow를 식별하는 운영 워크플로다. 본 도구는 #64에서 도입되었으며 기존 `netobs_drop_events_labeled_total` 의 workload 단위 emit 을 보완해 connection 단위 진단을 즉시 가능하게 한다.

## 메트릭 카탈로그

- `netobs_drop_events_flow_total{node, src_namespace, src_workload, traffic_scope, direction, drop_reason, drop_category, protocol, src_ip, src_port, dst_ip, dst_port}` counter. drop event의 5-tuple flow context emit
- `netobs_drop_burst:rate10s{src_namespace, src_pod, src_ip, src_port, dst_ip, dst_port, protocol, drop_reason, drop_category}` recording rule. 10초 윈도우 rate 산출

## 활성화 절차

`netobs_drop_events_flow_total` 은 cardinality 위험이 크다. 운영자가 진단 대상 namespace를 명시 등록해야 emit이 시작된다.

```sh
# DaemonSet env로 namespace allow-list 주입
kubectl -n ebpf-project set env daemonset/netobs-agent \
  NETOBS_DROP_FLOW_ALLOW_NAMESPACES=correlation-stress,my-app

# rollout 후 메트릭 확인
kubectl rollout status -n ebpf-project daemonset/netobs-agent
PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=netobs_drop_events_flow_total' | jq
```

빈 allow-list가 기본값이며 그 경우 emit 자체가 일어나지 않아 cardinality가 0 series로 유지된다.

## 카디널리티 분석

emit되는 series 수의 절대 상한은 다음으로 산정한다.

```
series 상한 = NETOBS_DROP_FLOW_MAX_ACTIVE × drop_reason 수
           = 1024 × 8 = 8,192 series per node
```

기본값 `NETOBS_DROP_FLOW_MAX_ACTIVE=1024` 는 LRU eviction으로 가장 오래된 flow가 자동 정리되어 본 한도를 절대 초과하지 않는다. 4 노드 cluster 기준 총 32k series 이내, 이슈 #64 의 100k 상한 안에 있다.

`NETOBS_DROP_FLOW_MAX_ACTIVE` 를 1024 외 값으로 운영자가 바꾸면 본 산식에 따라 series 상한이 비례한다.

## `NetObsDropBurst` alert runbook

alert가 발화하면 다음 4 단계로 root cause를 좁힌다.

### 1단계 — alert label에서 5-tuple 추출

alert label의 `src_namespace`, `src_pod`, `src_ip`, `src_port`, `dst_ip`, `dst_port`, `protocol`, `drop_reason` 8 필드가 burst의 정체성을 정의한다. 본 라벨 조합이 정확한 connection을 가리킨다.

### 2단계 — drop_reason 분류 확인

drop_category가 다음 4 종 중 어디에 속하는지 확인한다.

- `tx`: 송신 측 drop (NIC queue full, qdisc dropped, ICMP send fail 등). 노드 NIC 자원 압박 의심
- `rx`: 수신 측 drop (skb pulled, GRO drop 등). 수신 노드 또는 NIC 압박
- `netfilter`: conntrack / iptables / ipv6 disabled 등 정책 drop. cluster network policy 또는 noprol firewall 확인
- `tcp`: TCP retransmit timer, rst 등 transport 계층 drop

drop_category 별 의미는 [docs/netobs/drop-reason.md](drop-reason.md) 참고.

### 3단계 — 동시 신호 cross-reference

burst 시점의 다음 메트릭을 같은 시간대에 확인한다.

- `gpuobs_device_pcie_rx_bytes_per_second` 와 `gpuobs_device_pcie_tx_bytes_per_second`: GPU 노드의 PCIe 포화 여부
- `pod:cpu_throttle_score:5m`: src_pod의 cpu 압박
- `pod:memory_pressure_score:5m`: src_pod의 메모리 압박
- `correlation_noisy_neighbor_score{victim_pod=$src_pod}`: src_pod가 다른 워크로드의 영향을 받는지

### 4단계 — 합성 부하 검증

drop이 자연 발생이라면 #52 의 `workload-injector` 로 동일 src_pod에 합성 부하를 추가해 burst 패턴이 재현되는지 확인한다.

```sh
kubectl apply -f test/injector-examples/network.yaml  # network 부하 inject 후 burst 재현 확인
```

## 트러블슈팅

- **`netobs_drop_events_flow_total` 가 비어 있음**: `NETOBS_DROP_FLOW_ALLOW_NAMESPACES` 가 비어 있음. allow-list 등록 후 DaemonSet rollout 필요
- **burst alert가 5분 지나도 발화 안 함**: drop rate 가 10/s 임계 미만. 합성 시나리오로 검증하려면 iptables `-A FORWARD -d <pod_ip> -j DROP` 으로 sustained drop 발생
- **5-tuple 의 src_ip가 0.0.0.0 또는 dst_ip가 0.0.0.0**: socket이 bind 되기 전 drop. sk_protocol bitfield가 0 으로 emit되는 edge case로 본 series는 무시 가능
- **protocol 라벨이 "unknown"**: TCP / UDP 외 protocol (ICMP, GRE 등). 본 첫 구현은 TCP / UDP 한정이며 추후 확장 예정

## 비목표 (#64 의 follow-up)

- IPv6 5-tuple 은 별도 이슈에서 다룬다. 본 가이드는 IPv4 한정
- conntrack table dump의 자동 통합은 본 이슈 범위 밖이다
- drop 시점의 socket buffer 상태와 TCP 윈도우 추적은 본 이슈 범위 밖이다
