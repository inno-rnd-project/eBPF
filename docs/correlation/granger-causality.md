# Granger causality와 dominant dimension 운영 가이드

`correlation_noisy_neighbor_pvalue`와 `correlation_dominant_dimension`, `correlation_dominant_dimension_active:5m` 3종 메트릭으로 noisy neighbor 페어의 인과 방향을 통계적으로 검증하고 victim 단위 dominant cause를 자동 식별하는 운영 워크플로다. 본 도구는 #69에서 도입되었으며 기존 Pearson 상관계수만으로는 두 시계열 사이 인과 방향을 검증할 수 없던 한계를 Granger causality F-test와 dominant dimension weighting으로 보완한다.

## 메트릭과 alert 카탈로그

- `correlation_noisy_neighbor_pvalue{victim_namespace, victim_pod, victim_pod_uid, suspect_namespace, suspect_pod, suspect_pod_uid, resource_dimension, rank}` gauge. Granger causality p-value. 0.05 미만이면 src (suspect) 가 dst (victim latency) 를 Granger-cause 한다는 통계적 유의성이 강함
- `correlation_dominant_dimension{victim_namespace, victim_pod, victim_pod_uid, dimension}` gauge. victim 단위 4 dimension 중 sum 정규화 weight가 가장 큰 dimension 1종을 라벨로 노출. latency 압박과 무관하게 항상 emit되는 raw 메트릭
- `correlation_victim_latency_pressure:5m{victim_namespace, victim_pod, victim_pod_uid}` recording rule. victim의 latency p99가 0.1s 임계를 초과하면 1로 emit
- `correlation_dominant_dimension_active:5m{victim_namespace, victim_pod, victim_pod_uid, dimension}` recording rule. raw dominant_dimension을 latency pressure AND 게이팅으로 거른 active view

## Granger causality 산정식

두 시계열 x (suspect) 와 y (victim latency) 에 대해 lag order p로 다음 두 OLS 회귀 모델을 fit한다.

- restricted: `y_t = a0 + sum_{i=1..p} ai * y_{t-i}`
- unrestricted: `y_t = a0 + sum_{i=1..p} ai * y_{t-i} + sum_{j=1..p} bj * x_{t-j}`

두 모델의 잔차 제곱합 RSS_R과 RSS_U 차분으로 F-statistic을 산정한다.

```
F = ((RSS_R - RSS_U) / p) / (RSS_U / (n - 2p - 1))
```

p-value는 F 분포 (df1=p, df2=n-2p-1) 의 survival 함수로 산출한다. lag order p는 본 시리즈에서 2 고정이며 AIC와 BIC 자동 선택은 비목표다. 시계열의 stationarity test (ADF, KPSS) 도 비목표라 raw 시계열을 그대로 입력으로 받는다.

## dimension dominant 산정식

4 dimension (`cpu`, `gpu`, `memory`, `network`) 별로 victim의 NoisyNeighbor.Score max를 집계한 뒤 sum 정규화한다.

```
weight_i = score_i / sum(score)
```

dominant dimension은 weight + dimensionOffset 가산 후 max 1종을 채택하며 dimensionOffset은 enum 사전순 (`cpu` 4e-6, `gpu` 3e-6, `memory` 2e-6, `network` 1e-6) 으로 정확 동률 시 사전순 가장 앞 dimension이 우선 채택된다. 4 dimension 모두 score가 0인 victim은 결과에서 자연 제외되어 dashboard 빈 시리즈가 폭증하지 않는다.

## 진단 워크플로

1. dashboard `correlation noisy neighbor` 의 Panel 6 (Active dominant dimension) 에서 latency 압박을 받는 victim과 dominant cause 확인
2. Panel 7 (High-confidence noisy neighbors) 에서 동일 victim의 high-confidence suspect 페어와 dimension 확인. p-value < 0.05 필터링이 통계적 유의성이 약한 페어를 자동 제외
3. dominant dimension 별 drill-down

   - `cpu` dominant. `pod:cpu_throttle_score:5m`와 suspect의 cpu 사용량 timeline 비교
   - `memory` dominant. `pod:memory_pressure_score:5m`와 OOM 임박 여부 점검
   - `network` dominant. `pod:network_throughput_score:5m`와 NIC 포화 여부 점검
   - `gpu` dominant. `pod:gpu_memory_utilization_ratio:5m`와 GPU 측 자원 한계 점검

4. p-value가 모든 페어에서 0.05 이상으로 떨어진다면 통계적 유의성이 약한 상태이고 victim의 latency 압박이 noisy neighbor 외 다른 원인 (workload 자체 변동, 외부 트래픽 등) 일 가능성을 고려한다

## 검증 시나리오

dev cluster에서 `workload-injector`의 4 kind 합성 부하로 dominant dimension이 각 kind와 일치하는지 회귀 가드한다. 본 PR에서 `memory` kind를 신규 도입해 4 dimension 모두 검증 가능하다.

```sh
for kind in cpu memory network gpu; do
  kubectl apply -f test/injector-examples/${kind}.yaml
  kubectl wait --for=condition=complete --timeout=10m \
    -n ebpf-project job/workload-injector-${kind}-example
  PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
  kubectl exec -n monitoring $PROM_POD -c prometheus -- \
    wget -qO- "http://localhost:9090/api/v1/query?query=correlation_dominant_dimension_active:5m{dimension=\"${kind}\"}" | jq
  kubectl delete -f test/injector-examples/${kind}.yaml
done
```

각 kind에서 dominant dimension이 일치하면 회귀 통과로 본다. injector example 의 image 태그가 historical 값으로 박혀 있어 본 검증 전에 `sed -i "s|image: workload-injector:.*|image: workload-injector:$(cat VERSION)|" test/injector-examples/*.yaml` 로 현재 VERSION에 맞춰 조정한다.

## 카디널리티 분석

scrape 시점 시리즈 수 상한.

- `correlation_noisy_neighbor_pvalue`. 기존 score/lag와 동일 라벨 셋이라 같은 상한. 운영 환경 victim 수와 topN에 선형
- `correlation_dominant_dimension`. victim 수 x 1 dimension = victim 수
- `correlation_dominant_dimension_active:5m`. raw dominant 중 latency 압박 victim에 한정되어 raw보다 적거나 같음
- `correlation_victim_latency_pressure:5m`. latency 압박 victim 수

추가 카디널리티는 모두 active victim 수에 선형이라 cluster 단위 cap에 들어간다.

## 알려진 한계

- Bayesian causality와 do-calculus 같은 더 엄밀한 인과 추론은 본 시리즈 scope 외다. Granger causality까지가 책임이며 후속 시리즈에서 다룰 수 있다
- 시계열의 stationarity test 자동 변환 (ADF, KPSS) 은 본 시리즈 scope 외라 raw 시계열을 그대로 받는다. non-stationary 시계열의 spurious causality 위험은 운영자의 해석 단계에서 도메인 지식으로 판단한다
- lag order p는 2 고정이라 매우 긴 lag (수 분 이상) 의 인과는 잡지 못한다. 본 시리즈의 reconcile cycle (1h window 기준 2 step) 에 fit한 값이며 AIC/BIC 자동 선택은 follow-up이다
- dominant dimension의 latency pressure 임계 0.1s는 본 PR에서 PromQL recording rule 상수로 고정이라 운영자가 환경별로 다르게 운영하려면 `correlation_victim_latency_pressure:5m` rule의 표현식을 직접 수정한다
