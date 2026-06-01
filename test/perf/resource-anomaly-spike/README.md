# resource-anomaly-spike e2e 검증 시나리오

이슈 #89의 5분 z-score 기반 spike 감지가 dev cluster에서 정상 emit되는지 회귀 가드한다. dev cluster 전용이며 prod에서는 실행하지 않는다.

## 사전 조건

- dev cluster의 prometheus가 `monitoring` namespace에 ready
- #88의 `deploy/monitoring/`의 retention 60일 patch 적용 상태
- `netobs-gpuobs-correlation` PrometheusRule의 `netobs-gpuobs.resource-anomaly-spike.recording` group이 적용된 후 최소 30초 경과(base record는 즉시 emit, z-score는 7일 baseline 누적 필요)

## 시나리오 개요

5단계 회귀 가드를 동시 수행한다.

- 1차 가드: PrometheusRule의 `netobs-gpuobs.resource-anomaly-spike.recording` group 등록 확인
- 2차 가드: base record 4종(`cluster:gpu_util_5m_avg`와 `cluster:network_drop_5m_rate`와 `cluster:cpu_throttle_5m_avg`와 `cluster:memory_pressure_5m_avg`) 의 즉시 emit
- 3차 가드: z-score record 4종의 emit 확인(7일 baseline 누적 부족 시 graceful skip, count > 0 시 `clamp(-5, 5)` 가드 동작 확인)
- 4차 가드: alert rule 4종(`GPUUtilSpikeDetected`와 `NetworkDropSpikeDetected`와 `CPUThrottleSpikeDetected`와 `MemoryPressureSpikeDetected`) 등록 확인
- 5차 가드: alert 4종의 `severity=warning`과 `component=*-anomaly` 라벨 정합

## 실행

```sh
./verify.sh
```

## 종료 코드

- 0: 검증 통과(z-score 4종은 baseline 미달 시 skip으로 통과)
- 1: 검증 실패(group 미등록, record 부재, alert 미등록, label mismatch, clamp 가드 깨짐)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `SPIKE_NAMESPACE` | `ebpf-project` | PrometheusRule의 namespace |
| `SPIKE_PROM_NAMESPACE` | `monitoring` | prometheus Service의 namespace |
| `SPIKE_PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus Service 이름 |
| `SPIKE_PROM_PORT` | `9090` | prometheus Service port |
| `SPIKE_PROM_IP` | (auto) | prometheus ClusterIP 직접 지정(Service 자동 발견 우회) |

## 실패 시 진단

- `[fail] resource-anomaly-spike.recording group 미등록` 으로 떨어지면 `kubectl apply -f deploy/gpuobs/base/prometheus-rule.yaml` 재적용
- `[fail] cluster:*_5m_avg: count=0` 으로 떨어지면 base 시리즈(`gpuobs_device_utilization_percent`와 `netobs_drop_events_labeled_total`와 `pod:cpu_throttle_score:5m`와 `pod:memory_pressure_score:5m`) 의 emit 여부 확인
- `[skip] cluster:*_zscore:5m: count=0` 은 정상 동작. 7일 baseline 누적 후 자연 활성화
- `[fail] alert ... 미등록` 으로 떨어지면 prometheus-rule.yaml의 `netobs-gpuobs.alerts` group에 spike alert 4종 추가 확인
- `[fail] alert label 정합 깨짐` 으로 떨어지면 alert 4종의 `severity: warning`과 `component: *-anomaly` 매핑 검증(`docs/observability/resource-anomaly-spike.md` 참조)
- `[fail] clamp(-5, 5) 가드 깨짐` 으로 떨어지면 recording rule expr의 `clamp(..., -5, 5)` 부착 누락 확인
