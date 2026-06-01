# alert annotation 운영자 가이드

이슈 #90의 dashboard alert annotation 표시에 대한 운영자 가이드다. 7 dashboard의 시계열 패널에 alert 발화 시점을 vertical line으로 자동 표시해 운영자가 spike 시점에서 동시 발화 alert를 즉시 확인 가능하게 한다. 본 가이드는 severity별 iconColor 매핑과 dashboard별 alert scope 필터링 그리고 RCA 진입 흐름의 표준을 정리한다.

## 사용 시나리오

본 가이드는 RCA 가속 활용 흐름의 3 단계로 구성된다.

- 1단계 spike 발견: 시계열 패널에서 spike 또는 anomaly 시점을 식별
- 2단계 alert 식별: 같은 시점의 vertical line을 hover해 alertname과 severity 확인. red는 critical (즉시 대응 필요), orange는 warning (capacity planning 또는 trend 관찰 선행 신호)
- 3단계 RCA 진입: 식별된 alertname을 기반으로 #87의 panel link로 `rca-summarizer` 진입해 alert별 RCA 요약 확인

## severity별 iconColor 매핑

| severity | iconColor | 운영적 의미 |
|---|---|---|
| `critical` | red | 즉시 대응 필요. 운영 중단 또는 SLO 위반 직전 |
| `warning` | orange | capacity planning 또는 trend 관찰의 선행 신호. 즉시 대응 불요 |
| `info` (미적용) | (제외) | informational level로 vertical line 표시 가치 미흡. 본 표준에서 제외 |
| `none` (미적용) | (제외) | severity 미설정 alert (kube-prometheus 기본 alert 등). 본 표준에서 제외 |

## dashboard별 alert scope 필터링

같은 alert가 7 dashboard에 동시 vertical line으로 표시되면 운영자의 시각 피로가 누적되므로 `component` 라벨 기반 필터로 dashboard마다 자기 도메인 alert만 표시한다.

| dashboard | component 필터 | 의도 |
|---|---|---|
| `observability-overview` | 없음 (전체) | cluster 전체 진입점이라 모든 도메인 alert 표시 |
| `netobs-overview` | `component=~"netobs\|netobs-capacity\|observability"` | network 도메인 alert와 observability 인프라 alert 표시 |
| `gpuobs-overview` | `component=~"gpuobs\|gpuobs-capacity"` | GPU 도메인 alert와 capacity anomaly 표시 |
| `correlation-overview` | `component="correlation"` | correlation 분석 도메인 alert만 표시 |
| `gpu-network-correlation` | `component=~"netobs\|gpuobs\|netobs-capacity\|gpuobs-capacity"` | 두 도메인 cross-scope alert 표시 |
| `workload-injector` | `component="injector"` | 부하 주입 도메인 alert만 표시 |
| `rca-summarizer` | 없음 (전체) | alert-driven summary의 RCA 진입점이라 전체 alert 시간선 표시 |

## annotation의 표시 표준

7 dashboard의 annotation은 다음 공통 표준을 따른다.

- `expr`에 `ALERTS{alertstate="firing"}` 명시로 firing 상태만 표시 (pending은 noise 증폭 회피)
- `tagKeys: "severity,component,alertname"`로 Grafana의 annotation 검색과 필터 일관성 보장
- `titleFormat: "{{alertname}}"`로 hover 시 alertname 즉시 노출
- `textFormat: "severity={{severity}} component={{component}}"`로 추가 메타데이터 표시
- severity별 2 annotation rule(`alerts-critical`, `alerts-warning`) 분리 부착으로 iconColor 시각 구분

## RCA 진입 흐름

annotation 자체의 url 진입은 Grafana 12.x 표준에서 미지원이라 다음 흐름으로 RCA 진입한다.

- annotation hover로 alertname과 severity와 component 식별
- 같은 dashboard의 panel link (#87의 drill-down 표준) 활용
- `observability-overview`와 `netobs-overview`의 alert / anomaly 패널은 `rca-summarizer`로의 역방향 link 부착되어 있어 직접 진입 가능

## 후속 dashboard 추가 시 체크리스트

신규 dashboard를 추가할 때 다음 절차로 annotation 표준을 따른다.

- dashboard의 도메인 식별 (예: storage, security 등 신규 도메인)
- alert label의 `component` 라벨이 신규 도메인에 일치하는지 확인 (필요 시 PrometheusRule 추가 시 component 라벨 부착)
- 본 가이드의 dashboard별 alert scope 매핑 표에 1행 추가
- 신규 dashboard JSON의 `annotations.list`에 alerts-critical과 alerts-warning 2 rule 부착 (component 필터는 신규 도메인 매핑 적용)
- `test/perf/alert-annotation/verify.sh`의 `EXPECTED_ANNOTATION_COUNT`와 `EXPECTED_COMPONENT_FILTER` 매핑 갱신
- dev cluster apply 후 sidecar pickup 확인

## 비목표

- alert resolution annotation은 본 이슈 범위 밖으로 firing event 한정. resolution 표시는 별도 follow-up 이슈로 위임
- pending state annotation은 noise 증폭 위험으로 비목표 명시 (for 절 대기 중 vertical line 표시 회피)
- panel-level annotation scope (특정 패널에만 vertical line 표시) 는 Grafana 12.x 미지원으로 dashboard-level 전체 적용
- annotation의 historical retention 정책은 prometheus retention과 분리되어 본 이슈 변경 사항 없음
- annotation hover의 url 진입은 Grafana 12.x 표준 미지원으로 #87 panel link 위임
- `observability-overview`의 `Capacity trends` row (4주 / 30일 timeFrom) 에 annotation 시간 범위 정합 적용은 별도 annotation rule 신설 복잡도가 본 이슈 단순 "vertical line 표시" 목표와 분리 가능해 follow-up 이슈로 위임
- info severity와 none severity (Watchdog 등 kube-prometheus 기본 alert) 는 vertical line 표시 가치 미흡으로 본 표준에서 제외
