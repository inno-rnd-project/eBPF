# 간섭 영향 크기 (effect size) 운영 가이드

`correlation_noisy_neighbor_impact_seconds` 메트릭으로 noisy neighbor 간섭의 절대 영향 크기를 victim latency와 동일 단위(seconds)로 노출하는 운영 가이드다. 본 도구는 #146에서 도입되었으며 Pearson 상관계수(`correlation_noisy_neighbor_score`)가 "victim latency와 얼마나 동조하는가"의 강도만 보던 한계를 "압박이 victim을 실제로 얼마나 느리게 만들었는가"의 크기로 보완한다.

## 메트릭 카탈로그

- `correlation_noisy_neighbor_impact_seconds{victim_namespace, victim_pod, victim_pod_uid, suspect_namespace, suspect_pod, suspect_pod_uid, resource_dimension, rank}` gauge. suspect 압박 구간과 비압박 구간의 victim latency 차이(seconds). score와 동일한 8개 라벨 셋을 공유한다
- `/api/v1/noisy-neighbor` 응답의 각 항목에 `impact_seconds`와 `impact_ok` 필드로 함께 노출된다

## effect size 산정식

suspect 시계열 x와 victim latency 시계열 y에 대해 다음 차분으로 산정한다.

- x의 중앙값(median)을 임계로 두 구간으로 나눈다. `x > median`인 timestamp의 y를 high(압박) 구간, 나머지를 low(비압박) 구간으로 본다
- `impact = mean(y | high) - mean(y | low)`

회귀 기울기 대신 차분을 쓰는 이유는 suspect score가 0-1 정규화 값이라 기울기의 단위 해석이 모호한 반면, 차분은 "압박 시 vs 비압박 시 victim latency 차이"로 운영자가 직관적으로 읽을 수 있기 때문이다. victim latency 시계열이 이미 p99(`histogram_quantile(0.99, ...)`)이므로 차분은 압박 시 p99와 비압박 시 p99의 차이다.

가드 규칙은 다음과 같다.

- NaN과 Inf가 한쪽이라도 있는 timestamp는 pairwise 제거하고 length mismatch는 짧은 쪽으로 truncate한다 (Pearson과 동일 정책)
- high와 low 각 구간 표본이 effect size용 minSamples 미만이면 산정을 skip하고 `impact_ok=false`로 둔다. correlator는 Pearson 전체 표본 임계(`MinSamples`)의 1/4(최소 2)을 effect size용 minSamples로 넘겨, 짧은 window에서 양분된 각 구간이 임계를 채우게 한다. suspect가 상수라 분리가 안 되는 경우도 한 구간이 비어 자연히 가드에 걸린다
- 차이가 0 이하면 (압박이 latency를 줄이거나 영향이 없는 비-간섭 케이스) `impact_ok=false`로 둔다. collector는 `impact_ok=false` 시리즈를 emit하지 않아 0 noise가 끼지 않는다

## score와 impact의 조합 해석

score와 impact는 독립 지표라 운영자가 우선순위 판단에 함께 활용한다.

- score 높음 + impact 큼: suspect 압박이 victim latency와 강하게 동조하고 실제 손해도 크다. 최우선 조치 대상
- score 높음 + impact 작음: 동조는 강하나 절대 latency 손해는 작다. victim latency 자체가 낮은 구간이거나 영향이 미미한 경우
- score 낮음 + impact 큼: 단일 차분으로는 큰 차이가 보이나 시계열 전체 동조는 약하다. 산발적 spike나 다른 요인 동반 가능성을 함께 확인
- score 낮음 + impact 작음: 간섭 신호가 약하다

## dashboard 참조

`deploy/dashboards/correlation.json`의 "Top noisy neighbors" 패널이 score와 lag_seconds에 더해 impact_seconds 컬럼을 함께 노출한다. rank는 score 기준으로 부여되므로 impact 컬럼으로 정렬하면 동조 강도와 다른 "실제 latency 손해" 우선순위를 즉시 비교할 수 있다.

## 비목표

- AI 기반 영향 예측 모델 도입은 본 가이드 범위 밖이다
- cross-node interference layer의 effect size 산정은 본 도구 범위 밖이며 별도 follow-up으로 다룬다
