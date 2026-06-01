# capacity-trends 운영자 가이드

이슈 #88의 capacity-trends 패널 군에 대한 운영자 가이드다. `observability-overview` dashboard의 신규 row `Capacity trends`에 4 도메인 (GPU utilization, network throughput, CPU throttle, memory pressure) 의 heatmap과 multi-series overlay와 z-score 패널 3종씩 총 12 패널을 통합 시각화해 요일과 시간대 패턴 식별 그리고 baseline 대비 이상 감지를 dashboard 단일 진입점으로 제공한다.

## 사용 시나리오

본 가이드는 capacity planning 의사결정 흐름의 3 단계 시나리오를 따른다.

- 1단계 패턴 식별: heatmap 패널에서 매주 반복되는 요일과 시간대 패턴 (예: 매주 월요일 오전 9시 GPU spike) 을 색상 농도로 확인
- 2단계 이상 감지: z-score 패널에서 30일 baseline 대비 `|z| > 3` 임계 (정규분포 99.7% 신뢰구간 outside) 를 자동 highlight 해 normal shift 벗어난 시점을 식별
- 3단계 원인 추적: heatmap과 overlay 패널의 drill-down link (#87 표준 URL 패턴) 로 `netobs-overview` 또는 `gpuobs-overview` 또는 `correlation-overview` 진입해 noisy neighbor 식별. z-score 패널은 추가로 `rca-summarizer` 역방향 link로 RCA 요약 진입 경로도 제공. 본 row 의 패널은 cluster scope 라 시간 범위만 전파되고 node나 pod 단위 필터는 target dashboard 에서 재선택

## 패널 구성

| 도메인 | heatmap (요일×시간대) | overlay (4주 시계열) | z-score (30일 baseline) |
|---|---|---|---|
| GPU utilization | `cluster:gpu_util_1h_avg` | 동일 | `cluster:gpu_util_zscore:1h` |
| Network throughput | `cluster:network_1h_avg` | 동일 | `cluster:network_zscore:1h` |
| CPU throttle | `cluster:cpu_throttle_1h_avg` | 동일 | `cluster:cpu_throttle_zscore:1h` |
| Memory pressure | `cluster:memory_pressure_1h_avg` | 동일 | `cluster:memory_pressure_zscore:1h` |

각 도메인의 3 패널이 같은 row에 좌측 heatmap 중앙 overlay 우측 z-score로 배치된다.

## heatmap 해석

- X축: 시간대 (0-23시, KST 기준)
- Y축: 요일 (일-토)
- 색상: `Spectral` 8-step 스케일 (낮은 값은 파랑, 높은 값은 빨강)
- cell 단위 hover 시 정확한 값과 시간대 노출
- Grafana의 `calculate=true` 와 `timezone: Asia/Seoul` 설정으로 PromQL의 UTC 9시간 offset이 KST로 자동 보정

## z-score 임계의 운영적 의미

z-score는 `(current - 30일 baseline 평균) / 30일 baseline stddev`로 산출되며 baseline 대비 현재 값의 표준편차 거리를 의미한다.

- `|z| <= 1`: 정상 범위 (68% 신뢰구간 inside)
- `1 < |z| <= 3`: 약한 이상 신호 (95% 신뢰구간과 99.7% 신뢰구간 사이)
- `|z| > 3`: 강한 이상 신호 (99.7% 신뢰구간 outside, 3-sigma rule, normal shift)
- `|z| > 5`: 극단적 이상 (capacity 즉시 재검토 권장)

z-score는 `clamp(-5, 5)`로 시각화 안정성을 위해 산출 범위가 제한된다.

## timezone 처리 컨벤션

본 패널 군은 Prometheus의 UTC 기준 시계열을 Grafana의 panel-level `timezone: Asia/Seoul` 설정으로 KST 변환해 표시한다.

- recording rule (`deploy/gpuobs/base/prometheus-rule.yaml`) 의 PromQL은 UTC 그대로 산정
- heatmap과 overlay 패널의 `timezone: Asia/Seoul` 옵션으로 X축 시간대를 KST로 자동 변환
- alert rule (`GPUUtilAnomalyDetected` 등) 의 firing 시각은 Alertmanager의 timezone 설정 따름

## 4주 미만 운영 cluster의 graceful degradation

prometheus retention 60일 (`deploy/monitoring/prometheus-retention-patch.yaml`) 적용 직후에는 30일 baseline 산정에 필요한 데이터가 부족해 z-score 시계열이 비어있고 heatmap도 sparse 표시된다.

- heatmap의 `noValue` 메시지: "데이터 부족. capacity-trends 패널은 최소 4주의 메트릭 누적이 필요합니다"
- z-score record는 baseline window의 `(stddev > 0)` 가드로 시리즈 미 emit
- alert (`|z| > 3`) 도 record 부재로 자연 통과 (false positive 차단)
- retention 60일 적용 후 1주차에 1주 heatmap 부분 표시 시작, 4주차에 완전 표시, 30일차 이후 z-score 활성화

## 알림 4종의 발화 조건과 운영자 action 가이드

본 이슈가 신설한 alert 4종은 모두 `severity: warning` 그리고 `component: <domain>-capacity` 라벨을 부착한다. 즉시 대응 불요의 capacity planning 선행 신호로 throttle 류 critical alert와 운영 채널을 분리한다.

| alert | 조건 | component | 운영자 action |
|---|---|---|---|
| `GPUUtilAnomalyDetected` | `abs(cluster:gpu_util_zscore:1h) > 3` for 30m | `gpuobs-capacity` | heatmap에서 시간대 패턴 확인 후 GPU 자원 재할당 또는 워크로드 schedule 조정 검토 |
| `NetworkUtilAnomalyDetected` | `abs(cluster:network_zscore:1h) > 3` for 30m | `netobs-capacity` | overlay에서 시간대 분포 확인 후 NIC bandwidth 또는 Pod replica 재설정 검토 |
| `CPUThrottleAnomalyDetected` | `abs(cluster:cpu_throttle_zscore:1h) > 3` for 30m | `cpu-capacity` | heatmap에서 시간대 패턴 확인 후 CPU limit 상향 또는 워크로드 재배치 검토 |
| `MemoryPressureAnomalyDetected` | `abs(cluster:memory_pressure_zscore:1h) > 3` for 30m | `memory-capacity` | OOM 임박 위험으로 memory limit 상향 또는 워크로드 재배치 검토 |

## 후속 도메인 추가 시 체크리스트

본 capacity-trends row의 패턴은 다음 절차로 확장 가능하다.

- `deploy/gpuobs/base/prometheus-rule.yaml`의 `netobs-gpuobs.capacity-trends.recording` group에 신규 record 2종 추가 (`cluster:<domain>_1h_avg` 와 `cluster:<domain>_zscore:1h`)
- `deploy/dashboards/overview.json`의 Capacity trends row (id 600) 에 신규 panel 3종 추가 (heatmap + overlay + z-score, id 613-615 부터 +3 씩 증가)
- `netobs-gpuobs.alerts` group에 신규 alert 1종 추가 (`<Domain>AnomalyDetected`, severity warning, component `<domain>-capacity`)
- `test/perf/capacity-trends/verify.sh`의 EXPECTED_RECORDS 표 갱신

## 비목표

- 미래 예측 (forecasting): linear regression, holt-winters 같은 예측 모델은 본 이슈 범위 밖. PromQL의 `predict_linear` 활용한 단순 선형 외삽은 별도 follow-up 이슈로 위임
- disk 도메인: netobs / gpuobs agent가 disk 메트릭 미지원이므로 node-exporter 통합 follow-up 이슈에 위임
- Pod 단위 GPU utilization heatmap: RTX 3090 NVML 비지원 환경 제약으로 device scope (`gpuobs_device_utilization_percent`) 만 적용
- table row 클릭 단위 동적 drill-down: Grafana 12.x의 `panel.links`가 row scope 미지원으로 panel 단위 link만 적용
