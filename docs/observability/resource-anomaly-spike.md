# resource-anomaly-spike 운영자 가이드

이슈 #89의 5분 z-score 기반 자원 이상 징후 자동 highlight에 대한 운영자 가이드다. `observability-overview` dashboard의 신규 row `Resource anomaly spike`에 4 도메인(GPU utilization과 network drop rate와 CPU throttle과 memory pressure)의 5분 z-score를 timeseries 패널 4개로 시각화해 직전 7일 baseline 대비 즉시 outlier를 자동 highlight한다. #88의 1시간 z-score(`Capacity trends` row)가 장기 capacity planning 용도라면 본 row는 즉시 RCA spike 감지가 의도다.

## 사용 시나리오

본 가이드는 즉시 대응의 3단계 흐름을 따른다.

- 1단계 spike 감지: `Resource anomaly spike` row의 4 z-score 패널에서 `|z| > 3`(red) 또는 `|z| > 2`(orange) 색상으로 highlight된 시점 식별
- 2단계 alert 식별: alert annotation(`*-anomaly` component 라벨)의 vertical line과 시점이 일치하는지 #90의 annotation hover로 확인
- 3단계 RCA 진입: 패널의 drill-down link로 `netobs-overview` 또는 `gpuobs-overview` 또는 `correlation-overview` 또는 `rca-summarizer` 진입해 root cause 추적 후 워크로드 재배치 또는 limit 상향 검토

## 패널 구성

| 도메인 | base record | z-score record | alert |
|---|---|---|---|
| GPU utilization | `cluster:gpu_util_5m_avg` | `cluster:gpu_util_zscore:5m` | `GPUUtilSpikeDetected` |
| Network drop rate | `cluster:network_drop_5m_rate` | `cluster:network_drop_zscore:5m` | `NetworkDropSpikeDetected` |
| CPU throttle ratio | `cluster:cpu_throttle_5m_avg` | `cluster:cpu_throttle_zscore:5m` | `CPUThrottleSpikeDetected` |
| Memory pressure ratio | `cluster:memory_pressure_5m_avg` | `cluster:memory_pressure_zscore:5m` | `MemoryPressureSpikeDetected` |

4 패널이 2x2 grid로 배치된다.

## z-score 임계의 운영적 의미

z-score는 `(current 5분 평균 - 직전 7일 baseline 평균) / clamp_min(직전 7일 baseline stddev, floor)`로 산출되며 baseline 대비 현재 값의 표준편차 거리를 의미한다. 평탄한 baseline 케이스(stddev=0) 의 spike 감지 불가 risk 차단 위해 도메인별 floor(GPU util=1, network drop=0.1, CPU throttle=0.01, memory pressure=0.01) 로 stddev 최소값을 보장한다.

- `|z| <= 1`: 정상 범위(68% 신뢰구간 inside)
- `1 < |z| <= 2`: 약한 변동(green 표시 유지)
- `2 < |z| <= 3`: 약한 spike(orange highlight, 95% 신뢰구간과 99.7% 신뢰구간 사이)
- `|z| > 3`: 강한 spike(red highlight, 99.7% 신뢰구간 outside, 3-sigma rule, alert 발화 조건)
- `|z| > 5`: 극단적 이상(즉시 root cause 추적 권장)

z-score는 `clamp(-5, 5)`로 시각화 안정성을 위해 산출 범위가 제한된다.

## #88 capacity-trends 와의 시간 척도 분리

| 측면 | `Capacity trends` row (#88) | `Resource anomaly spike` row (#89) |
|---|---|---|
| 윈도우 | 1시간 평균 | 5분 평균 |
| baseline | 30일(`[30d] offset 30d`) | 7일(`[7d] offset 5m`) |
| 의도 | 장기 capacity planning | 즉시 RCA spike 감지 |
| alert sustained | 30분 | 5분 |
| alert severity | warning | warning |
| alert component | `*-capacity` | `*-anomaly` |

두 row가 같은 dashboard에 공존하지만 시간 척도와 component 라벨로 명확 분리되어 운영자 혼선이 차단된다.

## component 라벨 `*-anomaly` 의 의미

| component | 도메인 | dashboard annotation 노출 |
|---|---|---|
| `gpuobs-anomaly` | GPU utilization spike | `observability-overview`, `gpuobs-overview`, `gpu-network-correlation`, `rca-summarizer` |
| `netobs-anomaly` | Network drop rate spike | `observability-overview`, `netobs-overview`, `gpu-network-correlation`, `rca-summarizer` |
| `cpu-anomaly` | CPU throttle ratio spike | `observability-overview`, `rca-summarizer`(전체 노출) |
| `memory-anomaly` | Memory pressure ratio spike | `observability-overview`, `rca-summarizer`(전체 노출) |

## 4 alert의 발화 조건과 action 가이드

| alert | 조건 | action |
|---|---|---|
| `GPUUtilSpikeDetected` | `abs(cluster:gpu_util_zscore:5m) > 3` for 5m | GPU 워크로드의 root cause 추적 또는 GPU 자원 재배치 검토 |
| `NetworkDropSpikeDetected` | `abs(cluster:network_drop_zscore:5m) > 3` for 5m | netobs-overview의 drop_category 별 분포 확인 후 link quality 또는 NIC saturation 추적 |
| `CPUThrottleSpikeDetected` | `abs(cluster:cpu_throttle_zscore:5m) > 3` for 5m | CPU limit 상향 또는 워크로드 재배치 검토 |
| `MemoryPressureSpikeDetected` | `abs(cluster:memory_pressure_zscore:5m) > 3` for 5m | memory limit 상향 또는 워크로드 재배치 검토(OOM 임박 위험) |

## workload-injector 합성 부하 검증 절차

dev cluster의 `loadgens.injector.netobs.io` CRD(`internal/injector/loadgen/loadgen.go`의 `Kind*` 정의) 로 본 이슈의 4 도메인 중 3 도메인을 합성 부하로 검증 가능하다.

- `KindCPU` 워크로드 적용 시 `CPUThrottleSpikeDetected` 발화 확인
- `KindNetwork` 워크로드 적용 시 `NetworkDropSpikeDetected` 발화 확인(드롭 패턴이 명확한 케이스 한정)
- `KindGPU` 워크로드 적용 시 `GPUUtilSpikeDetected` 발화 확인
- `KindMemory` 는 OOM 위험이 있어 자동 verify.sh 대신 manual 검증으로 분리. 합성 부하 시 `MemoryPressureSpikeDetected` 발화 가능

본 검증은 직전 7일 baseline 누적 이후에 가장 안정적이다. baseline 데이터가 일부만 누적된 케이스 에서도 clamp_min floor 적용으로 z-score 산정 자체는 가능하지만 baseline의 신뢰도가 낮아 false positive 위험이 있다.

## 4주 미만 운영 cluster의 graceful degradation

prometheus retention 60일(`deploy/monitoring/prometheus-retention-patch.yaml`) 적용 직후에는 직전 7일 baseline 산정에 필요한 데이터가 부족해 z-score 결과의 신뢰도가 낮다.

- baseline 데이터가 전혀 없는 신규 배포 직후 케이스: `avg_over_time` 자체가 vector 미 emit 이라 z-score 도 자연 미 emit
- baseline 일부 누적 (1일치 등) 케이스: clamp_min floor 가 stddev 평탄 risk 차단 으로 z-score 산정 가능. 단 baseline 표준 분포 가 충분히 형성 되지 않아 false positive 위험 있음
- retention 60일 적용 후 7일차 부터 z-score 신뢰도 안정화. 그 전까지 dashboard 의 alert annotation 결과 는 참고용

## 후속 도메인 추가 시 체크리스트

본 row의 패턴은 다음 절차로 확장 가능하다.

- `deploy/gpuobs/base/prometheus-rule-capacity-anomaly.yaml`의 `netobs-gpuobs.resource-anomaly-spike.recording` group에 신규 record 2종 추가(`cluster:<domain>_<metric>_5m_avg/rate`와 `cluster:<domain>_<metric>_zscore:5m`)
- `netobs-gpuobs.alerts` group에 신규 alert 1종 추가(`<Domain>SpikeDetected`, severity warning, component `<domain>-anomaly`)
- `deploy/dashboards/overview.json`의 `Resource anomaly spike` row(id 700)에 신규 panel 1종 추가
- `deploy/dashboards/<관련>.json`의 annotation expr에 `<domain>-anomaly` component 추가
- `test/perf/alert-annotation/verify.sh`의 `EXPECTED_COMPONENT_FILTER`와 `test/perf/resource-anomaly-spike/verify.sh`의 기대 record / alert 매핑 갱신
- 본 가이드의 패널 구성 표와 component 라벨 표 갱신

## 비목표

- 머신러닝 기반 anomaly detection은 본 이슈 범위 밖(이슈 원안 명시). z-score statistical baseline 한정
- 다중 변수 anomaly correlation은 #84의 cross-node interference 확장으로 별도 follow-up 이슈로 위임
- self-health 메트릭(BPF map utilization과 ringbuf drops) 의 5분 z-score는 본 row의 4 도메인과 신호 도메인이 다르므로 별도 follow-up
- 1시간 z-score(#88)와 5분 z-score(본 이슈)의 record 통합은 시간 척도 분리 의도와 충돌해 본 이슈 변경 사항 없음
- `*-capacity`와 `*-anomaly` component 라벨의 통합은 #88과 #90의 기존 매핑 표와 충돌해 분리 유지
