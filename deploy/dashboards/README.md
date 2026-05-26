# dashboard 본질 라벨 셋 표준

본 디렉터리의 Grafana dashboard 들이 따르는 PromQL 그룹화 표준을 정의한다. DaemonSet agent
(`netobs-agent`, `gpuobs-agent`) 가 재배포될 때마다 ServiceMonitor 가 자동 부여하는 `pod` 와
`instance` 라벨이 매 재시작마다 새 값으로 emit 되어 Prometheus 가 별개 시리즈로 인식해
누적된다. 본질 라벨 (메트릭이 가리키는 실체를 식별하는 라벨) 로 그룹화하면 active 시리즈와
stale 시리즈가 단일 line 으로 통합되어 dashboard 에 의도된 1 시리즈만 보인다 (#76).

raw 메트릭의 `pod` 와 `instance` 라벨은 troubleshooting 추적용으로 Prometheus 에 그대로
노출 유지한다. ServiceMonitor `metricRelabelings` 로 라벨 자체를 drop 하는 근본 해결은 본
표준 범위 밖이며 별도 follow-up 이슈로 추적한다.

## 메트릭 카테고리별 본질 라벨

| 카테고리 | 본질 라벨 셋 | 집계 함수 |
|---|---|---|
| device 메트릭 (gpu temperature / utilization / clock / throttle 등) | `node, gpu_uuid, gpu_index, gpu_model` | gauge 는 `max by(...)`, counter 는 `sum by(...)` |
| Pod 메트릭 (cuda_h2d_bytes / kernel_launches / pod_bytes 등) | `node, src_namespace, src_pod, src_pod_uid` (또는 `src_workload` 단위) | gauge 는 `max by(...)`, counter 는 `sum by(...)` |
| stage / drop / TCP 메트릭 (netobs 의 stage_latency / drop / retrans 등) | `stage, src_namespace, src_workload, traffic_scope, direction` 등 메트릭별 분리 | counter 는 `sum by(...)`, histogram 은 `histogram_quantile + sum by(le, ...)` |
| BPF self-health (program_loaded / ringbuf_drops / map_utilization / informer_sync_lag) | `node, symbol` 또는 `node, map` 또는 `node` | `max by(...)` |
| NVML self-health (nvml_call_duration / nvml_errors / informer_sync_lag) | `node, call` 또는 `node, call, error_code` 또는 `node` | gauge 는 `max by(...)`, histogram 은 `sum by(le, node, call)` |
| RCA 메트릭 (rca_summary_emitted_total / rca_summary_last_summary_info) | `alert_name` 또는 `alert_name, dominant_dimension, top_suspect, primary_drop_flow` | `max by(...)` 또는 `sum by(alert_name)` |
| correlation 메트릭 (correlation_noisy_neighbor_score / lag / pvalue / dominant_dimension) | `victim_namespace, victim_pod, suspect_namespace, suspect_pod, resource_dimension` 등 메트릭별 분리 | `max by(...)` |
| injector 메트릭 (injector_active / blast_radius_score / runs_total) | `target_namespace, target_pod, kind` 또는 `target_namespace, target_pod, kind, victim_namespace, victim_pod` | gauge 는 `max by(...)`, counter 는 `sum by(...)` |
| up 메트릭 | `job` (replica 단위 stale instance 통합) | `max by(job)` 또는 `max by(job, node)` (DaemonSet 의 node 식별이 필요한 경우) |

## 집계 함수 선택 규칙

- `max by(...)`: gauge 메트릭의 default. active 시리즈와 stale 시리즈가 같은 본질 라벨이라
  값이 같으면 결과가 변하지 않고, stale 시리즈가 마지막 값에 머문 케이스에서도 active 값을
  올바르게 노출한다
- `avg by(...)`: gauge 메트릭 중 본질 라벨 셋 안에 여러 sample 이 의도적으로 존재하는 경우
  (예: 단일 node 안 여러 GPU 의 평균 온도) 만 사용한다
- `sum by(...)`: counter 메트릭의 default. 재배포로 분리된 counter 시리즈를 더해 누적값을
  복원한다
- `histogram_quantile(q, sum by(le, ...))`: histogram 의 quantile 산정. `le` 라벨이 본질
  라벨에 항상 포함되어야 한다

## 적용 전후 PromQL 예시

### device 메트릭 (gauge)

```promql
# 전 (재배포마다 stale 시리즈가 새 line 으로 노출됨)
gpuobs_device_temperature_celsius{node=~"$node", gpu_uuid=~"$gpu_uuid"}

# 후 (본질 라벨 그룹화로 1 line 통합)
max by(node, gpu_uuid, gpu_index, gpu_model) (
  gpuobs_device_temperature_celsius{node=~"$node", gpu_uuid=~"$gpu_uuid"}
)
```

### Pod 메트릭 (counter rate)

```promql
# 전
rate(gpuobs_cuda_h2d_bytes_total{node=~"$node", src_pod=~"$src_pod"}[5m])

# 후
sum by(node, src_namespace, src_pod, gpu_uuid) (
  rate(gpuobs_cuda_h2d_bytes_total{node=~"$node", src_pod=~"$src_pod"}[5m])
)
```

### histogram p99

```promql
# 전
histogram_quantile(0.99, rate(gpuobs_nvml_call_duration_seconds_bucket[5m]))

# 후
histogram_quantile(0.99, sum by(le, call) (
  rate(gpuobs_nvml_call_duration_seconds_bucket[5m])
))
```

## 회귀 가드

dashboard 변경 시 dev cluster 에서 시간 범위를 24 시간으로 두고 다음 패널의 line 수가 기대값
과 일치하는지 확인한다.

- device 메트릭 패널: GPU 수 (dev cluster RTX 3090 1 대 환경에서는 1 line)
- Pod 메트릭 패널: active Pod 수 (`correlation-stress` 의 victim / suspect-async / suspect-sync /
  client 등)
- BPF self-health 메트릭: `(symbol, map)` 라벨 차원 × node 수
- NVML self-health 메트릭: call 수 × node 수
- RCA 메트릭: 현재 firing 중인 alert 수
- correlation 메트릭: active victim 수 × dimension 수 × Top-N rank
