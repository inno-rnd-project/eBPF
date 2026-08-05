# capacity-trends e2e 검증 시나리오

이슈 #88의 capacity-trends 패널 군이 dev cluster에서 정상 emit되는지 회귀 가드한다. dev cluster 전용이며 prod에서는 실행하지 않는다.

## 사전 조건

- dev cluster의 prometheus가 `monitoring` namespace에 ready
- `deploy/monitoring/` 의 retention 60일 patch가 적용된 상태 (`kubectl apply -k deploy/monitoring/`)
- `netobs-gpuobs-correlation` PrometheusRule의 `netobs-gpuobs.capacity-trends.recording` group이 적용된 후 최소 1분 경과 (1시간 윈도우 평균이라 evaluation 직후 채워짐)
- z-score 시리즈는 30일 baseline 누적이 필요. retention 60일 적용 후 1주차에 시작, 30일차 이후 완전 활성화

## 시나리오 개요

- 1차 가드: prometheus retention이 60d로 적용된 상태 확인
- 2차 가드: PrometheusRule의 `netobs-gpuobs.capacity-trends.recording` group 등록 확인
- 3차 가드: avg record 4종 (`cluster:gpu_util_1h_avg`, `cluster:network_1h_avg`, `cluster:cpu_throttle_1h_avg`, `cluster:memory_pressure_1h_avg`) 의 동시 emit (count > 0)
- 4차 가드: z-score record 4종의 emit 확인 (baseline 미달 시 graceful skip, count > 0 시 `clamp(-5, 5)` 가드 동작 확인)
- 5차 가드: alert rule 4종 (`GPUUtilAnomalyDetected`, `NetworkUtilAnomalyDetected`, `CPUThrottleAnomalyDetected`, `MemoryPressureAnomalyDetected`) 등록 확인

## 실행

```sh
./verify.sh
```

## 종료 코드

- 0: 검증 통과 (z-score 4종은 baseline 미달 시 skip 으로 통과)
- 1: 검증 실패 (retention 미적용, record 부재, alert 미등록, clamp 가드 깨짐)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `CAP_NAMESPACE` | `ebpf-project` | PrometheusRule의 namespace |
| `CAP_PROM_NAMESPACE` | `monitoring` | prometheus Service의 namespace |
| `CAP_PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus Service 이름 |
| `CAP_PROM_PORT` | `9090` | prometheus Service port |
| `CAP_PROM_IP` | (auto) | prometheus ClusterIP 직접 지정 (Service 자동 발견 우회) |

## 실패 시 진단

- `[fail] prometheus retention=... 가 60d 와 정합 안 됨` 으로 떨어지면 `kubectl apply -k deploy/monitoring/` 적용 필요. 또는 `kubectl patch prometheus -n monitoring kube-prometheus-stack-prometheus --type merge -p '{"spec":{"retention":"60d"}}'` 직접 patch
- `[fail] capacity-trends.recording group 미등록` 으로 떨어지면 `kubectl apply -f deploy/gpuobs/base/prometheus-rule-capacity-anomaly.yaml` 재적용
- `[fail] cluster:*_1h_avg: count=0` 으로 떨어지면 base 시리즈 (`gpuobs_device_utilization_percent`, `pod:network_throughput_score:5m`, `pod:cpu_throttle_score:5m`, `pod:memory_pressure_score:5m`) 의 emit 여부 확인
- `[skip] cluster:*_zscore:1h: count=0` 은 정상 동작. 30일 baseline 누적 후 자연 활성화
- `[fail] alert ... 미등록` 으로 떨어지면 prometheus-rule.yaml의 alerts group에 capacity anomaly alert 4종 추가 확인
