# observability overview 운영 가이드

cluster 의 이상 신호를 4 단계 (`cluster → node → pod → root cause`) 로 좁혀가는 운영 워크플로다. uid `observability-overview` 의 Grafana 통합 대시보드 한 화면 안에서 dropdown 만으로 drill-down 한다.

## 4 단계 워크플로

### 단계 1 — cluster overview

대시보드 상단의 4 single-stat (`GPU health`, `CPU health`, `Network health`, `Memory health`) 가 입력이다. score 는 `cluster:<카테고리>_health_score:5m` recording rule 의 산출값으로 0 ~ 1 범위 (1 healthy, 0 worst) 다.

| 색상 | 임계 | 운영 판단 |
|---|---|---|
| green | 0.85 이상 | 정상 |
| yellow | 0.7 ~ 0.85 | 관찰 (5 분 후 재확인) |
| orange | 0.5 ~ 0.7 | 조사 시작 (단계 2 로 drill-down) |
| red | 0.5 미만 | 즉시 대응 (alert summary 동시 확인) |

`Cluster timeline` row 의 4 카테고리 시계열에서 anomaly 시점과 `ALERTS{alertstate="firing"}` annotation 의 일치 확인. annotation 의 tag 라벨이 `severity` / `component` / `alertname` 을 매핑해 클릭 시 발화 alert 가 노출된다.

### 단계 2 — node 좁힘

`Node` variable dropdown 에서 의심 노드 선택 후 `Node drill-down` row 의 4 패널을 본다.

- `Node cpu_throttle_score (Pod 별)`: 노드의 어느 Pod 가 cpu 압박 받는지
- `Node memory_pressure_score (Pod 별)`: 동일 패턴의 메모리 압박
- `Node GPU utilization (device 별)`: GPU 사용률 시계열. GPU 없는 노드는 N/A
- `Node drop / retrans rate (drop_category 별)`: 네트워크 drop / 재전송 rate

`Active alerts on node ($node)` 테이블이 해당 노드의 firing alert 를 보여준다. severity 별 색상 분기로 critical / warning / info 우선순위 식별.

### 단계 3 — pod 좁힘

`Pod` variable dropdown 에서 단계 2 의 의심 Pod 선택 후 `Pod drill-down` row 의 4 패널을 본다.

- `Stage latency p99`: 본 Pod 의 stage 별 latency p99 시계열
- `GPU memory used (Pod)`: GPU 사용 시 메모리 점유량. 미사용 Pod 는 N/A
- `Noisy neighbor Top-N (victim=$pod)`: `correlation_noisy_neighbor_score` 의 dimension 별 suspect 순위. score 0.85 이상 빨강 / 0.7 주황 / 0.5 노랑 임계 색상으로 즉시 식별
- `Pod 별 RX/TX 대역폭 (Mbps, $src_pod)` (#130): raw counter `netobs_pod_bytes_total` 의 layer l4 rate를 PromQL 단에서 Mbps 환산한 dual-direction 시계열. egress (TX, blue) 와 ingress (RX, green) 별 분리 표시

#### Pod 별 RX/TX 대역폭 panel 의 활용

본 panel은 Pod 단위 네트워크 대역폭을 Mbps 단위로 즉시 가시화해 운영자가 다음 시나리오에서 활용 가능하다.

- **대역폭 spike 식별**: TX 의 sustained burst가 NIC capacity (`netobs_node_nic_capacity_bytes_per_sec`) 의 50% 이상이면 cross-pod 간섭 의심. `Noisy neighbor Top-N` 표의 network dimension 신호와 cross-reference
- **RX/TX 비대칭 패턴 인식**: ingress 가 egress 대비 과도하게 높으면 본 Pod가 receive 측 (예: API 수신 서버) 이고 그 반대면 send 측 (예: 미디어 송신). 워크로드 성격에 따른 자연 패턴 비교
- **drop spike와의 cross-reference**: 같은 시점의 `Node drop / retrans rate (drop_category 별)` panel과 비교. 대역폭 burst가 drop spike를 동반하면 NIC saturation 또는 queue overflow 의심

본 panel 의 query 는 raw counter `netobs_pod_bytes_total` 의 rate를 PromQL 단에서 직접 계산해 신규 recording rule 도입 없이 direction 라벨 분리를 활용한다. unit `Mbits` 설정으로 Grafana 가 Kbps와 Gbps 표기를 자동 SI prefix 변환한다.

본 panel의 비목표는 다음과 같다.

- per-flow 5-tuple 분리는 본 panel 외 (`drop-flow` dashboard와 `netobs_flow_bytes_total` 메트릭 참조)
- nic layer 의 raw counter 표시는 본 panel 외 (`layer="l4"` 필터 고정으로 cardinality 폐쇄)
- alert 발화 threshold 도입은 본 panel 외 (별도 follow-up)

### 단계 4 — root cause 분석

단계 3 의 noisy neighbor Top-N 에서 강한 suspect 페어를 발견하면 두 도구로 확정한다.

- `correlation-debug` CLI 로 1 회성 재현 (#50 참고). 같은 시점의 자연 발생 페어 검증
- `workload-injector` 로 suspect Pod 에 명시 부하 발사 (#52 참고). `correlation_blast_radius_score` 가 victim 측 latency 변동을 정량화

## alert label → dashboard variable 매핑

운영자가 alert 의 label 로 받은 정보를 dashboard 의 variable 에 어떻게 매핑하는지의 단일 표다.

| Alert | severity | label 키 | variable |
|---|---|---|---|
| `NetObsHighStageLatencyP99` | warning | `node`, `src_namespace`, `src_workload`, `stage` | `$node` (직접), `$pod` (src_workload 로부터 추정) |
| `NetObsHighDropRate` | warning | `node`, `drop_category`, `src_namespace`, `src_workload` | `$node` (직접), drop_category 는 dashboard panel 의 legend 로 |
| `NetObsHighRetransmissionRate` | warning | `node`, `src_namespace`, `src_workload` | `$node` (직접) |
| `GPUObsThrottleActive` | warning | `node`, `gpu_uuid`, `reason` | `$node` (직접), gpu_uuid 는 panel legend |
| `GPUObsThermalHeadroomLow` | warning | `node`, `gpu_uuid`, `threshold` | `$node` (직접) |
| `GPUObsPowerHeadroomLow` | warning | `node`, `gpu_uuid` | `$node` (직접) |
| `GPUIdleWithPCIeSaturation` | warning | `node` | `$node` (직접) |
| `GPUIdleWithNetworkPressure` | warning | `node` | `$node` (직접) |
| `GPUIdleWithCPUThrottle` | warning | `node`, `src_namespace`, `src_pod` | `$node`, `$pod` 둘 다 직접 |
| `GPUIdleWithMemoryPressure` | warning | `node`, `src_namespace`, `src_pod` | `$node`, `$pod` 둘 다 직접 |
| `GPUIdleWithHostComputeStall` | warning | `node`, `src_namespace`, `src_pod` | `$node`, `$pod` 둘 다 직접 |
| `CorrelationStrongNoisyNeighbor` | warning | `victim_namespace`, `victim_pod`, `suspect_namespace`, `suspect_pod`, `resource_dimension` | `$pod` = `victim_pod`, suspect 는 Pod drill-down 의 Top-N 테이블에서 식별 |
| `CorrelationExporterStalled` | warning | n/a (인프라 alert) | dashboard 가 아닌 `kubectl logs` 로 직접 진단 |
| `CorrelationExporterReconcileErrors` | warning | n/a (인프라 alert) | 동일 |
| `InjectorActive` | info | `target_namespace`, `target_pod`, `target_node`, `kind` | `$node` = `target_node`, `$pod` = `target_pod` |
| `BlastRadiusHigh` | warning | `target_pod`, `kind`, `victim_pod`, `victim_namespace` | `$pod` = `victim_pod`, suspect 는 `target_pod` |
| `InjectorStuck` | critical | `target_namespace`, `target_pod`, `kind` | `$pod` = `target_pod` |

## 시나리오별 진입점 row

dashboard 하단에 운영 시나리오별 collapsed row를 두어 운영자가 4 단계 워크플로 외에 시나리오 단위로도 진단을 시작할 수 있다. collapsed 상태로 시작하므로 row 헤더를 클릭해 펼친 뒤 사용한다.

- **GPU idle 진단 진입점** (id 800): `pod:gpu_idle_cause_weight:5m` 의 5 cause weight stacked bar와 `cluster:gpu_idle_dominant_cause:5m` dominant cause indicator로 구성된다. dominant cause를 식별한 뒤 row의 `correlation-overview` drill-down link로 이동해 noisy neighbor 페어를 확정한다
- **Drop spike 추적 진입점** (id 810): `netobs_drop_burst:rate1m` 의 5-tuple별 burst rate 시계열과 node와 drop_category별 drop rate 분포로 구성된다. burst를 일으키는 connection을 식별한 뒤 row의 `netobs-overview` drill-down link로 이동해 drop reason과 kernel stack을 확정한다
- **Capacity 계획 진입점**: 별도 신규 row 없이 기존 `Capacity trends` row (id 600) 와 `Resource anomaly spike` row (id 700) 를 진입점으로 활용한다. 4주 heatmap의 시간대 패턴과 z-score 추세 이탈로 capacity 선행 신호를 본다

세 시나리오의 전체 흐름은 [ONBOARDING.md](../ONBOARDING.md)의 주요 운영 시나리오 절에서 단계별 표로 안내한다.

## annotation 의 활용

dashboard 의 모든 timeline 패널에 `ALERTS{alertstate="firing"}` 가 annotation 으로 표시된다. 시점이 health score 의 anomaly 와 일치하면 그 alert 가 변동 원인일 가능성이 높다. annotation 클릭 시 tag 라벨 (`severity`, `component`, `alertname`) 이 노출되어 단계 2 의 variable 선택을 즉시 결정 가능하다.

## 트러블슈팅

- **single-stat 이 N/A 로 보임**: `cluster:gpu_health_score:5m` 등 recording rule 의 base series 부재. GPU 미설치 cluster 또는 traffic 없는 dev cluster 에서 정상 동작. promtool 검증과 별개로 Prometheus 에서 base 메트릭이 emit 되는지 직접 query 로 확인
- **node variable 이 비어 있음**: `node:gpu_util_p95:5m` recording rule 이 산출하지 않는 환경. 대안으로 variable query 를 `label_values(up{job=~".*agent.*"}, node)` 으로 임시 교체 가능
- **pod variable 이 비어 있음**: 선택한 node 에 `netobs_pod_stage_latency_labeled_seconds_count` 메트릭이 없음. POD_METRICS_ENABLED 환경 변수가 false 면 발생
- **annotation 이 표시 안 됨**: cluster 에 firing alert 가 없을 때 정상. `kubectl exec` 로 prometheus 에 직접 query 하면 빈 결과 확인 가능

## 패널 제목 스코프 표기 규칙

pod 스코프 신호와 node 스코프 신호가 한 화면에 공존하므로 (예: node 스코프 memory health 가 1.0 인데 pod 스코프 memory_pressure 상위 pod 가 동시에 표시), 스코프 오독을 막기 위해 패널 표기는 다음 규칙을 따른다 (#310).

- pod 스코프 신호 (`pod:*_score:5m` 등) 의 제목에 `Node` 접두를 쓰지 않는다. `Pod <신호명> (<분모>, $node)` 형태로 스코프와 분모를 제목에 드러내고, 노드 필터 변수 (`$node`) 는 스코프가 아니라 필터임을 제목 괄호로 표시한다
- 신호의 분모 (pod limit 대비인지 노드 실측인지) 와 "1.0 근접이 무엇을 뜻하는지" 를 description 에 명시한다. 노드 여유와 무관한 pod limit 대비 신호는 그 무관함을 함께 적는다
- 불감대나 clamp 같은 비선형 매핑이 있는 신호 (memory health 의 위험 구간 매핑 등) 는 매핑 구간을 description 에 명시해 "왜 항상 1.0 인가" 류의 오독을 막는다
