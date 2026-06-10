# RCA 매핑 multi-source cross-reference

## 배경

`cmd/rca-summarizer`의 webhook 수신 흐름과 alert별 RCA mapping 9종은 이미 완성되어 있으나 RCA summary 생성 알고리즘이 단일 source 휴리스틱 수준이라 correlation 분석 결과와 netobs 분석 결과와 gpuobs 분석 결과의 multi-source cross-reference 부재로 신뢰도 가시화가 제한적이었다. #122에서 동일 root cause가 다중 도메인에서 동시 신호로 나타날 때 본 신호를 가중치 합산해 RCA confidence score (0-1) 를 산출하고 false positive guard로 score 미달 alert의 metrics emit을 skip하는 흐름을 도입한다.

## multi-source seed 우선순위

본 구현은 deterministic 가중치 합산 알고리즘으로 동작한다. AI 기반 root cause 예측은 본 PR 범위 밖이다.

| Source | 가중치 | 근거 | 정규화 식 |
|---|---|---|---|
| correlation | 0.5 | correlation-exporter의 Pearson score는 두 시계열의 quantitative 동조 강도라 가장 강한 root cause indicator | `max by(victim) (noisy_neighbor.score)` 의 0-1 정규화 값 |
| netobs | 0.3 | drop flow rate는 root cause indicator보다 symptom 신호에 가까워 보조 가중치 | `max(rate(netobs_drop_burst:rate1m)) / 100` 의 clamp 0-1 |
| gpuobs | 0.2 | GPU idle cause weight는 4 dimension의 normalize 분포라 cross-reference 보조 신호 | `max by(node) (pod:gpu_idle_cause_weight:5m)` 의 0-1 값 |

세 가중치 합산이 1.0이라 모든 source가 최대 신호 (1.0) 일 때 confidence도 1.0에 도달한다. 단일 source 만으로는 가중치 만큼의 최대 confidence (예: correlation 단독 0.5) 에 머무른다.

## confidence score의 운영 의미

`rca_summary_confidence_score{alert_name, dominant_dimension}` gauge로 노출되는 0-1 정규화 값이며 다음 운영 해석을 갖는다.

- `score >= 0.7` 강한 cross-reference 합치. 다중 도메인이 동일 root cause를 가리키고 있어 RCA summary의 top_suspect와 dominant_dimension이 운영 결정의 1차 근거로 활용 가능
- `0.3 <= score < 0.7` 단일 도메인 또는 부분 cross-reference 만 일치. RCA summary는 starting point로 활용하고 추가 검증 (dashboard drill-down) 필요
- `score < 0.3` false positive 의심. 본 임계 미달 alert는 `rca_summary_emitted_total` counter가 증가하지 않고 `rca_summary_skipped_total{reason="below_threshold"}` counter만 갱신된다. `/rca?alert=<name>` JSON endpoint에는 store entry가 유지되어 운영자가 직접 조회 가능하다

## 9 alert별 cross-reference 흐름

각 mapping이 활용 가능한 source 셋은 alert 라벨이 victim Pod와 node를 어디까지 노출하는지에 따라 다르다.

| Alert | victim 식별 | 활용 source | 비고 |
|---|---|---|---|
| `NetObsDropBurst` | `src_namespace`와 `src_pod` | 3 source 모두 | 5-tuple drop flow는 primary_drop_flow에 직접 직렬화 |
| `GPUObsCudaStreamWaitHigh` | `src_namespace`와 `src_pod` | 3 source 모두 | neighbor[0].dimension이 dominant_dimension을 overwrite 가능 |
| `GPUObsThermalThrottleSustained` | `node`와 `gpu_uuid` | gpuobs source 만 | victim Pod 미식별이라 correlation과 netobs factor는 자연 0 |
| `GPUIdleWithPCIeSaturation` | `src_namespace`와 `src_pod` | 3 source 모두 | dimension network |
| `GPUIdleWithNetworkPressure` | `src_namespace`와 `src_pod` | 3 source 모두 | dimension network |
| `GPUIdleWithCPUThrottle` | `src_namespace`와 `src_pod` | 3 source 모두 | dimension cpu |
| `GPUIdleWithMemoryPressure` | `src_namespace`와 `src_pod` | 3 source 모두 | dimension memory |
| `GPUIdleWithHostComputeStall` | `src_namespace`와 `src_pod` | 3 source 모두 | dimension cpu |
| `CorrelationStrongNoisyNeighbor` | `victim_namespace`와 `victim_pod` | 3 source 모두 | alert 라벨의 suspect는 그대로 유지, cross-validation 목적 |

`GPUObsThermalThrottleSustained`는 victim Pod 식별이 불가하므로 confidence가 최대 `WeightGpuobs` (0.2) 에 머무른다. 본 PR의 `RCA_CONFIDENCE_THRESHOLD` 와 `-confidence-threshold` 는 전역 설정이라 alert별 override는 미지원하며 기본 threshold 0.3 환경에서는 본 alert의 metrics emit이 항상 skip된다. metrics 가시화가 필요한 운영자는 전역 threshold를 0.2 이하로 낮춰 운영하거나 thermal 도메인 전용 dashboard 흐름과 `/rca?alert=GPUObsThermalThrottleSustained` JSON endpoint의 store entry를 활용한다. alert별 threshold override 도입은 별도 follow-up 이슈로 둔다.

## threshold 자동 튜닝 절차

본 PR은 정적 hardcoded threshold (기본 0.3) 만 지원하며 dynamic tuning은 별도 follow-up 이슈로 둔다. 운영자가 환경별 false positive 비율을 확인 후 다음 절차로 수동 조정한다.

- `rca_summary_skipped_total / rca_summary_emitted_total` 비율 1주일 추세 확인. 비율이 너무 높으면 threshold가 과도하게 높음
- alert별 `rca_summary_confidence_score` 시계열을 `quantile_over_time(0.5, ...[1w])` 와 `quantile_over_time(0.95, ...[1w])` 으로 p50과 p95 산출. 본 메트릭은 gauge라 histogram_quantile은 적용 불가하며 range-vector quantile을 사용한다. p95가 threshold보다 낮으면 대부분 alert가 skip
- threshold 조정. `RCA_CONFIDENCE_THRESHOLD` env 갱신 또는 `-confidence-threshold` flag로 재배포

자동 튜닝 (예: percentile 기반 자가 조정) 은 본 PR 범위 외다.

## false positive guard 의 실패 진단

- `rca_summary_skipped_total{alert_name=X, reason="below_threshold"}` 가 급증 시 X alert의 multi-source cross-reference 흐름 점검. correlation snapshot fetch 실패 또는 Prometheus query timeout 가능성 확인
- `rca_summary_emitted_total` 이 0 으로 머무름. threshold가 과도하게 높거나 모든 source fetch 가 0 신호 반환 중. dev cluster의 active workload 부재 환경에서는 정상 동작
- `rca_summary_confidence_score{alert_name=X}` gauge 값이 매 webhook 마다 동일. source fetch가 캐시 stale 상태일 가능성. snapshot TTL (90초) 만료 대기 또는 correlation-exporter reconcile 주기 확인

## 비목표

- AI 또는 머신러닝 기반 root cause 예측은 본 PR 외
- multi-source seed의 dynamic threshold tuning은 별도 이슈
- RCA summary의 자연어 생성 강화는 본 PR 외
- 가중치 정책의 환경별 분기 (예: gpuobs 전용 환경에서 gpuobs 가중치 0.5 로 상향) 는 별도 이슈
