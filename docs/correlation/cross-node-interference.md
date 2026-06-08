# cross-node interference layer

## 배경

#51의 `correlation-exporter` noisy neighbor 모델은 동일 노드의 pod 쌍에 대해서만 (`victim_pod`, `suspect_pod`) 페어를 분석한다. multi-node cluster에서 한 노드의 자원 압박이 다른 노드의 pod latency에 영향을 주는 케이스 (cluster 공유 storage, shared NIC, cross-node network 등) 는 본 모델 의 분석 범위 밖이다. 본 PR은 #84의 cross-node interference layer를 도입해 node 단위 자원 압박과 다른 노드 latency 사이의 상관을 별도 메트릭으로 노출한다.

## 입도 정책

본 layer는 **node 단위 분석으로 단독 도입**한다. pod-level noisy neighbor 메트릭, `PodIdentity`, `NoisyNeighbor`, `neighborLabels` 셋은 일절 변경하지 않는다. 동일 cycle에서 두 입도가 같은 데이터를 두 번 산출하지 않도록 pair enumeration과 selection도 독립 함수 (`EnumerateNodePairs`, `SelectTopNCrossNode`) 로 둔다.

## dimension 의미

기존 pod-level 4 dimension (`cpu`, `memory`, `network`, `gpu`) 을 노드 레벨로 rollup한 4종 score를 신설한다. `correlation_cross_node_score`의 `dimension` 라벨은 본 4종 값으로 emit되며 `classifyDimension` 휴리스틱이 그대로 재사용된다. cross-node correlator의 입력은 본 4종 dimension score를 직접 사용하고 (`Config.CrossNodeMetrics` 필드 참고) 별도 통합 score (`node:pressure_score:5m`) 는 운영자가 노드 단위 압박을 단일 시계열로 dashboard / 수동 query에서 즉시 보기 위한 보조 시리즈로만 노출한다.

## recording rule 정의

`deploy/gpuobs/base/prometheus-rule.yaml`의 `netobs-gpuobs.recording` 그룹에 다음 6종 을 추가한다.

| record name | expr 요약 | 의미 |
|---|---|---|
| `node:cpu_pressure_score:5m` | `max by(node) (pod:cpu_throttle_score:5m)` | 노드 의 가장 큰 cpu throttle 압박 |
| `node:memory_pressure_score:5m` | `max by(node) (pod:memory_pressure_score:5m)` | 노드 의 가장 큰 memory 압박 |
| `node:network_pressure_score:5m` | `max by(node) (pod:network_throughput_score:5m or pod:network_retrans_score:5m)` | 노드 의 가장 큰 network 압박 |
| `node:gpu_pressure_score:5m` | `max by(node) (pod:host_compute_stall_score:5m)` | 노드 의 가장 큰 host-GPU compute stall 압박 |
| `node:pressure_score:5m` | 4 dimension 평균 정규화 | 노드 단위 통합 압박 (보조 시리즈, correlator 입력 아님) |
| `node:netobs_pod_stage_latency_p99:5m` | `histogram_quantile(0.99, sum by(node, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))` | 노드 단위 p99 latency (cross-node victim 식별용) |

## 분석 layer 구성

- `EnumerateNodePairs(items []LabeledSeries) []NodePair`: `node` 라벨 한정 시계열을 입력 받아 `victim_node != suspect_node`인 페어만 생성한다. 노드 단위 cross-product 라 dev cluster 4노드 기준 12 페어, prod 수십 노드 도 수백 페어 로 cardinality 부담 이 거의 없다.
- `SelectTopNCrossNode(results []CorrelationResult, topN int) []NodeInterference`: `(victim_node, dimension)` 그룹화 후 `(victim_node, suspect_node, dimension)` 단일 키 로 max score dedup해 rank 부여한다. `victim_node == suspect_node`는 enumerate 단에서 이미 제외되어 selection에서는 별도 가드가 불요하다.
- `CorrelationResult.IsCrossNode`: caller 가 same-node와 cross-node 결과를 동일 슬라이스에서 분기 식별 가능하도록 한다. exporter 가 본 플래그 로 emit 메트릭을 분기한다.

## 신규 메트릭

```
correlation_cross_node_score{victim_node, suspect_node, dimension}
```

dimension 라벨 4종 × 노드 페어 수 (n × (n-1)) 로 cardinality 가 cap된다. dev cluster 4 노드 기준 48 series, prod 16 노드 가정 시 960 series 로 series 폭주 위험 이 없다. 기존 `correlation_noisy_neighbor_*` 메트릭 셋 은 변경 없이 그대로 emit 된다.

## opt-in 정책

`internal/correlation/config.go`의 `CrossNodeEnabled bool` (default false) 로 본 layer 를 명시 opt-in 한다. `CrossNodeMaxPairs int`는 reserved 옵션 으로 두어 추후 pod-level cross-node 확장 시 활용 가능 하게 한다. `cmd/correlation-exporter/main.go`의 `-cross-node` flag 로 동일 토글 을 노출 한다.

## 비목표

- cluster 외부 (서로 다른 cluster 간) 영향 분석은 본 PR 범위 밖이다
- node 자체 의 hardware 고장 진단은 본 PR 범위 밖이다 (kube_node_status_condition 등 기존 신호 활용)
- dashboard drill-down navigation은 #87, 시간대별 추이 분석은 #88, z-score highlight는 #89, alert annotation은 #90 의 follow-up

## API 활용 (#119)

`correlation-exporter` 가 `/api/v1/cross-node-interference` endpoint 로 cross-node interference snapshot 을 JSON 으로 노출 한다. 외부 시스템 (RCA summarizer 와 운영 자동화) 이 본 endpoint 를 polling 해 Prometheus query 재계산 없이 in-memory snapshot 을 활용 가능 하다. dashboard 의 `Top cross-node interference` panel 도 동일 데이터 셋 을 표 형식 으로 표시 한다.

### query 파라미터

| 파라미터 | 타입 | 기본 | 설명 |
|---|---|---|---|
| `victim_node` | string | 무필터 | victim node 이름 정확 일치 필터 |
| `suspect_node` | string | 무필터 | suspect node 이름 정확 일치 필터 |
| `dimension` | enum | 무필터 | `cpu` 와 `memory` 와 `network` 와 `gpu` 중 택일 |
| `rank_max` | int | 무제한 | (victim_node, dimension) 그룹 내 max rank cap |
| `limit` | int | `100` | 응답 item 최대 개수 (최대 1000) |
| `offset` | int | `0` | 응답 시작 offset |

### 응답 schema

```json
{
  "items": [
    {
      "victim_node": "gpu",
      "victim_metric": "node:netobs_pod_stage_latency_p99:5m",
      "suspect_node": "ebpf-worker1",
      "suspect_metric": "node:cpu_pressure_score:5m",
      "dimension": "cpu",
      "rank": 1,
      "score": 0.82,
      "lag_steps": 1,
      "sample_count": 60,
      "p_value": 0.012,
      "granger_ok": true
    }
  ],
  "page": {
    "limit": 100,
    "offset": 0,
    "total": 4
  }
}
```

### curl 예시

특정 victim node 의 모든 suspect 조회:

```sh
curl -sf "http://correlation-exporter.ebpf-project:9830/api/v1/cross-node-interference?victim_node=gpu" | jq
```

특정 dimension 의 TopN (rank 5 이내) 만 조회:

```sh
curl -sf "http://correlation-exporter.ebpf-project:9830/api/v1/cross-node-interference?dimension=cpu&rank_max=5" | jq
```

특정 victim 과 suspect pair 의 score 시계열 확인 (단일 페어):

```sh
curl -sf "http://correlation-exporter.ebpf-project:9830/api/v1/cross-node-interference?victim_node=gpu&suspect_node=ebpf-worker1" | jq
```

### RCA summarizer 연계

`rca-summarizer` 가 alert webhook handler 에서 victim_node 단위 cross-node interference 후보 를 본 endpoint 로 조회 해 root cause 분석 prompt 에 첨부 한다. exporter 의 in-memory snapshot 을 그대로 활용 하므로 Prometheus query 재계산 비용 이 없 고 webhook 응답 30s 임계 를 안정 적 으로 통과 한다.

## 회귀 검증

dev cluster의 한 노드 (`ebpf-worker1`) 에 workload-injector cpu Kind 부하를 인가하고 다른 노드 (`gpu` 또는 `ebpf-worker2`) 에 latency-sensitive workload를 두어 `correlation_cross_node_score{victim_node="gpu",suspect_node="ebpf-worker1",dimension="cpu"}` 가 임계 (`test/perf/cross-node/verify.sh` 의 `CROSS_NODE_THRESHOLD` 기본값 0.3) 이상으로 산정되는지 회귀 가드한다. `victim_node == suspect_node`인 시리즈가 0 개임을 함께 확인한다. `verify.sh` 의 3 차 가드 (#119) 가 신규 `/api/v1/cross-node-interference` endpoint 의 정상 응답 과 JSON schema 정합 도 fail-on-miss 로 검증 한다. 가드 통과 직후 두 워크로드는 `kubectl delete`로 정리해 dev cluster 상주를 회피한다.
