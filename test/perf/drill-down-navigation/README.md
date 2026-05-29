# drill-down navigation e2e 검증 시나리오

이슈 #87 의 cluster-node-pod 계층 drill-down navigation 통합이 dev cluster 에서 정상 source-of-truth 상태인지 회귀 가드한다. dev cluster 전용이며 prod 에서는 실행하지 않는다.

## 사전 조건

- dev cluster 에 `deploy/dashboards/` 가 `kubectl apply -k` 로 배포된 상태
- 7 dashboard ConfigMap 이 `ebpf-project` namespace 에 존재 (`overview-dashboard`, `netobs-dashboard`, `gpuobs-dashboard`, `correlation-dashboard`, `gpu-network-correlation-dashboard`, `injector-dashboard`, `rca-dashboard`)

## 시나리오 개요

- Grafana API 의 auth 의존성을 배제하고 sidecar 의 source-of-truth 인 ConfigMap 자체를 inspect 해 panel.links 정합성을 검증
- 각 dashboard 별 link 총 개수 가 매핑 표와 일치 + 모든 link URL 에 `${__url_time_range}` 부착 여부 가드
- 검증 항목 2종:
  - 1차: link 총 개수 == 기대값 (`overview=45`, `netobs=19`, `gpuobs=21`, `correlation=2`, `gpu-network-correlation=3`, `injector=12`, `rca=0`)
  - 2차: 모든 link URL 에 `${__url_time_range}` macro 부착

## 실행

```sh
./verify.sh
```

## 종료 코드

- 0: 검증 통과
- 1: 검증 실패 (link 개수 불일치, `${__url_time_range}` 미부착, ConfigMap 부재 등)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `DRILLNAV_NAMESPACE` | `ebpf-project` | dashboard ConfigMap 의 namespace |

## 실패 시 진단

- `[fail] {cm}: links={actual} expected={expected}` 으로 떨어지면 source 파일 (`deploy/dashboards/{name}.json`) 의 panel.links 가 매핑 표와 정합한지 확인. 의도적 변경이면 본 verify.sh 의 `EXPECTED_LINKS` 와 `docs/observability/drill-down-navigation.md` 의 출발지/도착지 매트릭스 동시 갱신
- `[fail] {cm}: ... 중 N개에 ${__url_time_range} 미부착` 으로 떨어지면 신규 link 의 URL 표준 누락. URL 끝에 `&${__url_time_range}` 추가
- ConfigMap 부재 시 `kubectl apply -k deploy/dashboards/` 재배포 필요
