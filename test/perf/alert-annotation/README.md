# alert-annotation e2e 검증 시나리오

이슈 #90의 dashboard alert annotation 표시가 dev cluster에서 정합 상태인지 회귀 가드한다. dev cluster 전용이며 prod에서는 실행하지 않는다.

## 사전 조건

- dev cluster에 `deploy/dashboards/`가 `kubectl apply -k`로 배포된 상태
- 7 dashboard ConfigMap이 `ebpf-project` namespace에 존재 (`overview-dashboard`, `netobs-dashboard`, `gpuobs-dashboard`, `correlation-dashboard`, `gpu-network-correlation-dashboard`, `injector-dashboard`, `rca-dashboard`)

## 시나리오 개요

Grafana API의 auth 의존성을 회피하기 위해 sidecar의 source-of-truth인 ConfigMap 자체를 inspect해 annotation 정의를 검증한다. 7 dashboard 모두 다음 5 항목을 동시 가드한다.

- annotation 개수 == 2 (alerts-critical + alerts-warning)
- 모든 annotation의 `expr`에 `alertstate="firing"` 부착 (pending 제외 표준)
- `alerts-critical` annotation의 `iconColor=red`와 `severity="critical"` 필터 매칭
- `alerts-warning` annotation의 `iconColor=orange`와 `severity="warning"` 필터 매칭
- dashboard별 `component` 라벨 필터가 매핑 표와 정합 (`observability-overview`와 `rca-dashboard`는 전체 필터 미적용)

## 실행

```sh
./verify.sh
```

## 종료 코드

- 0: 검증 통과
- 1: 검증 실패 (annotation 누락, severity/iconColor 불일치, component 필터 mismatch, ConfigMap 부재 등)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `ANNOT_NAMESPACE` | `ebpf-project` | dashboard ConfigMap의 namespace |

## component 필터 매핑 표

본 verify.sh의 `EXPECTED_COMPONENT_FILTER` 와 일치해야 한다. 신규 dashboard 추가 시 본 표와 verify.sh의 `EXPECTED_ANNOTATION_COUNT` 그리고 `EXPECTED_COMPONENT_FILTER` 두 매핑을 동시 갱신한다.

| dashboard | component 필터 |
|---|---|
| `overview-dashboard` | 없음 (전체) |
| `netobs-dashboard` | `component=~"netobs\|netobs-capacity\|netobs-anomaly\|observability"` |
| `gpuobs-dashboard` | `component=~"gpuobs\|gpuobs-capacity\|gpuobs-anomaly"` |
| `correlation-dashboard` | `component="correlation"` |
| `gpu-network-correlation-dashboard` | `component=~"netobs\|gpuobs\|netobs-capacity\|gpuobs-capacity\|netobs-anomaly\|gpuobs-anomaly"` |
| `injector-dashboard` | `component="injector"` |
| `rca-dashboard` | 없음 (전체, RCA 진입점 특례) |

## 실패 시 진단

- `[fail] {cm}: annotation 개수=...` 으로 떨어지면 dashboard JSON의 `annotations.list`가 2 rule (alerts-critical + alerts-warning) 부착 여부 확인. source 파일 (`deploy/dashboards/{name}.json`) 수정 후 `kubectl apply -k deploy/dashboards/` 재배포
- `[fail] {cm}: alertstate="firing" 부착=...` 으로 떨어지면 annotation의 `expr`에 `alertstate="firing"` 누락 확인. pending 제외 표준 적용 필수
- `[fail] {cm}: alerts-critical (red)` 또는 `alerts-warning (orange)` mismatch는 severity별 iconColor 매핑 표준 (`docs/observability/alert-annotation.md` 참조) 확인
- `[fail] {cm}: component 필터 ... 매칭=...` 으로 떨어지면 본 README의 매핑 표와 dashboard의 annotation expr 동시 점검. 의도적 변경이면 본 README와 verify.sh의 `EXPECTED_COMPONENT_FILTER` 동시 갱신. annotation 개수 변경이 동반되면 `EXPECTED_ANNOTATION_COUNT`도 함께 갱신
