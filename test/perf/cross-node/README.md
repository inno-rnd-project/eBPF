# cross-node interference e2e 검증 시나리오

이슈 #84의 cross-node interference layer 가 dev cluster에서 정상 emit되는지 회귀 가드한다. `test/perf/rca-e2e` 와 동일 패턴이며 dev cluster 전용으로 prod에서는 실행하지 않는다.

## 사전 조건

- dev cluster의 `correlation-exporter` Deployment가 #84 series의 image로 업데이트되어 있음
- `correlation-exporter`에 `CROSS_NODE=true` env 또는 `--cross-node` flag가 활성화되어 있음 (default false)
- `node:cpu_pressure_score:5m` 와 `node:netobs_pod_stage_latency_p99:5m` recording rule이 prometheus에 deploy되어 있음
- `kube-prometheus-stack-prometheus` Service가 monitoring namespace에 ready
- 본 스크립트 실행 호스트가 dev cluster의 Service CIDR에 routable

## 시나리오 개요

- suspect_node (default `ebpf-worker1`) 에 workload-injector cpu Kind 부하 인가
- victim_node (default `gpu`) 의 latency-sensitive workload가 cross-node 영향을 받는지 측정
- `correlation_cross_node_score{victim_node="gpu",suspect_node="ebpf-worker1",dimension="cpu"}` 가 임계 (default 0.3) 이상으로 산정되는지 회귀 가드

stress Pod의 노드는 manifest의 `TARGET_NODE` env로 강제 지정한다. workload-injector가 `TARGET_NODE` 미지정 시 victim의 Pod의 node로 fallback하지만 본 시나리오는 victim과 suspect가 다른 노드에 위치하는 cross-node interference가 의도이므로 명시 지정이 필수다.

## 실행

```sh
./verify.sh
```

본 스크립트는 trigger 적용 후 최대 540초 (15초 간격 polling) 안에 cross_node_score가 임계 이상으로 도달하는지 확인한다. `victim_node == suspect_node` 시리즈가 0개임을 함께 1차 가드한다. 마지막에는 메모리 규칙에 따라 stress Job을 자동 정리한다.

## 종료 코드

- 0: 검증 통과 (`cross_node_score >= threshold`, `victim_node == suspect_node` 시리즈 0개)
- 1: 검증 실패 (timeout, prometheus 미접근, opt-in 비활성, self-loop 시리즈 존재 등)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `CROSS_NODE_NAMESPACE` | `ebpf-project` | stress Job이 동작하는 namespace |
| `CROSS_NODE_VICTIM` | `gpu` | victim_node 라벨 값 |
| `CROSS_NODE_SUSPECT` | `ebpf-worker1` | suspect_node 라벨 값 (stress 노드와 일치) |
| `CROSS_NODE_THRESHOLD` | `0.3` | cross_node_score 통과 임계 |
| `CROSS_NODE_TIMEOUT` | `540` | 통과 대기 timeout 초 |
| `CROSS_NODE_POLL_INTERVAL` | `15` | prometheus polling 주기 초 |
| `CROSS_NODE_PROM_NAMESPACE` | `monitoring` | prometheus Service namespace |
| `CROSS_NODE_PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus Service 이름 |
| `CROSS_NODE_PROM_PORT` | `9090` | prometheus Service port |

## 실패 시 진단

`[fail] timed out` 으로 떨어지면 다음 순서로 점검한다.

- `correlation-exporter` 의 env에 `CROSS_NODE=true` 또는 args에 `--cross-node` 가 포함되어 있는지
- `node:cpu_pressure_score:5m` 와 `node:netobs_pod_stage_latency_p99:5m` 시리즈가 prometheus에서 직접 query로 조회되는지
- `correlation_reconcile_pairs_total` 가 증가하는지 (reconcile cycle 자체가 동작하는지)
- victim_node에 latency-sensitive workload (예: `correlation-stress/victim`) 가 실제로 띄워져 있어 latency p99 시계열이 emit되는지

`[fail] victim_node == suspect_node 시리즈가 존재한다` 로 떨어지면 EnumerateNodePairs의 same-node 가드 회귀가 의심된다. 단위 테스트 `TestEnumerateNodePairs_ExcludesSameNode` 가드 통과 여부를 우선 확인하라.
