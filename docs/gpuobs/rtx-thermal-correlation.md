# RTX consumer GPU thermal 과 power 와 clock 상관 분석 운영 가이드

`gpuobs_thermal_correlation_score:5m`, `gpuobs_clock_drop_score:5m`, `GPUObsThermalThrottleSustained` alert 와 dashboard cross-reference 패널 2종으로 RTX consumer GPU 의 thermal throttle / power / clock 상관관계를 자동 진단하는 운영 워크플로다. 본 도구는 #68 에서 도입되었으며 dev cluster 의 RTX 3090 처럼 dynamic boost clock 과 thermal throttle 이 자주 발생하는 consumer card 의 운영 특성을 활용한다.

## 메트릭과 alert 카탈로그

- `gpuobs_thermal_correlation_score:5m{node, gpu_uuid}` gauge. temperature 와 hw_thermal_slowdown throttle 의 5분 윈도우 lag 0 Pearson 절댓값. `[0, 1]` 단조 score 로 0 은 무상관, 1 은 강한 양상관
- `gpuobs_clock_drop_score:5m{node, gpu_uuid}` gauge. graphics clock 의 현재 값과 5분 max 비율. 1.0 은 정상 boost, 임계 미만은 throttle 영향으로 하향
- `gpuobs_thermal_temp_avg:5m`, `gpuobs_thermal_throttle_avg:5m`, `gpuobs_thermal_x_throttle_avg:5m`, `gpuobs_thermal_temp_stddev:5m`, `gpuobs_thermal_throttle_stddev:5m` 5종 helper. Pearson 정의식 입력
- `GPUObsThermalThrottleSustained` alert. `gpuobs_thermal_correlation_score:5m > 0.8` 이 10분 지속이고 같은 10분 윈도우 안에 hw_thermal_slowdown throttle 이 발생한 적 있는 경우 발화

## Pearson 산정식과 throttle binary 의 처리

본 PR 의 산정식은 PromQL 단독으로 풀어 쓴 Pearson 정의식이다.

```
r = (avg(xy) - avg(x) * avg(y)) / (stddev(x) * stddev(y))
```

`x` 는 `gpuobs_device_temperature_celsius`, `y` 는 `gpuobs_device_throttle_active{reason="hw_thermal_slowdown"}` 이다. throttle 이 0/1 binary 라 엄밀히는 point-biserial correlation 이고 음양 방향 모두 같은 강도로 의미가 있는 신호라 `abs()` 로 절댓값화해 `[0, 1]` 단조 score 로 노출한다. 분모 `(stddev_temp * stddev_throttle) > 0` 가드로 평탄 시간대 (변동 없음) 의 NaN 시리즈가 라벨 세트에 끼지 않게 한다.

`internal/correlation` 라이브러리는 cross-correlation 관례에 따라 lag step 별 산출과 EnumeratePairs, correlation-exporter Collector 체인으로 동작해 GPU 단위 단일 score 의 본 PR 형태와 다르므로 reuse 대상에서 제외했다.

## 진단 워크플로

1. alert 발화 시 payload 의 `node` 와 `gpu_uuid` 로 영향 GPU 식별
2. dashboard `gpuobs` 의 thermal cross-reference 패널 2종 확인
   - 좌측 축 temperature 와 우측 축 power 의 dual-axis 패널에서 thermal slowdown threshold 임계까지 올라가는 시간대와 power 증가 시점이 일치하는지 점검
   - graphics clock 과 hw_thermal_slowdown throttle 의 dual-axis 패널에서 clock 하향과 throttle 활성이 동시에 발생하는지 점검
3. score 양쪽 비교 쿼리

   ```sh
   PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
   kubectl exec -n monitoring $PROM_POD -c prometheus -- \
     wget -qO- 'http://localhost:9090/api/v1/query?query=gpuobs_thermal_correlation_score:5m' | jq
   kubectl exec -n monitoring $PROM_POD -c prometheus -- \
     wget -qO- 'http://localhost:9090/api/v1/query?query=gpuobs_clock_drop_score:5m' | jq
   ```

4. 조치 방향 분류

   - thermal_correlation 높음 + clock_drop 낮음 (0.8 미만). thermal 영향이 강한 clock 하향이 진행 중. 냉각 / 팬 커브 / ambient 점검
   - thermal_correlation 높음 + clock_drop 1.0 근접. throttle 이 활성이지만 clock 영향까지는 아직. 임계 도달 직전 상태
   - thermal_correlation 낮음 + clock_drop 낮음. thermal 외 원인 (power throttle 등) 으로 clock 하향. `gpuobs_device_throttle_active` 의 다른 reason 시리즈 점검

## 검증 시나리오

dev cluster 의 RTX 3090 자연 발열로 thermal slowdown 을 유도해 alert 와 score 가 정상 산정되는지 회귀 가드한다.

```sh
kubectl apply -f test/perf/pytorch-resnet50-bench.yaml
```

10 분 이상 부하 후 throttle 발생을 확인한다.

```sh
PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=gpuobs_device_throttle_active{reason="hw_thermal_slowdown"}' | jq
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=gpuobs_thermal_correlation_score:5m' | jq
```

throttle 이 1 로 올라온 시점이 있고 score 가 0.5 이상으로 emit 되면 회귀 통과로 본다. 가드 통과 직후 bench Pod 를 정리한다.

```sh
kubectl delete -f test/perf/pytorch-resnet50-bench.yaml
```

## 카디널리티 분석

scrape 시점 시리즈 수 상한.

- helper 5종 x GPU 수 = 5 시리즈 (단일 GPU 클러스터)
- score 2종 x GPU 수 = 2 시리즈
- 총 7 시리즈 / GPU

multi-GPU 노드에서도 GPU 수에 선형 증가하며 cluster 단위 cap 에 충분히 들어간다.

## 알려진 한계

- 데이터센터 GPU 의 전력 capping 분석 (`gpuobs_device_power_limit_enforced_watts` 변동) 은 본 PR 범위 밖이다. consumer card 한정 첫 구현이라 follow-up 으로 분리한다
- multi-GPU 노드의 GPU 간 thermal 영향 (인접 GPU 의 가열) 은 본 PR 범위 밖이다. dev cluster 의 single GPU 전제로 시작한다
- throttle 이 0/1 binary 라 Pearson 이 엄밀히는 point-biserial correlation 이다. abs() 절댓값과 game-only 패턴 (양상관 강도) 으로 의미를 좁혔다
- `gpuobs_clock_drop_score:5m` 의 도메인은 graphics 한 가지로 고정. sm / mem / video 의 thermal 영향 추적은 follow-up 으로 분리
- RTX 3090 의 hw_thermal_slowdown threshold 는 95°C 이고 정상 냉각 환경에서 ResNet50 inference 부하만으로는 도달이 어렵다. 검증 시점에 throttle 이 변동하지 않으면 stddev=0 으로 `thermal_correlation_score:5m` 의 분모 가드가 시리즈를 자연 skip 한다. 본 상태는 의도된 동작이며 실제 throttle 이 한 번이라도 발생한 시간대만 score 가 emit 된다
