# alert-routing e2e 회귀 가드 (#106)

이슈 #106 의 AlertmanagerConfig routing tree 4 분기 skeleton 도입을 dev cluster 에서 검증하는 회귀 가드 스크립트다. 외부 채널 (Slack / SMTP / PagerDuty) 통합 부재 환경 이라 실제 channel 수신 검증은 본 가드 범위 외 이며 routing tree 의 구조 정합과 라벨 매칭 의도만 자동 검증한다.

## 실행

```sh
test/perf/alert-routing/verify.sh
```

env 로 override 가능.

- `ROUTE_TIMEOUT` (기본 180s): configYAML 정합 polling timeout
- `ALERT_NAMESPACE` (기본 `ebpf-project`): AlertmanagerConfig CRD 의 namespace
- `ALERTMGR_NAMESPACE` (기본 `monitoring`): Alertmanager Pod 의 namespace
- `ALERTMGR_POD` (기본 `alertmanager-kube-prometheus-stack-alertmanager-0`): amtool 실행 대상 Pod

## 가드 단계

- 1차 (fail-on-miss): `kubectl get alertmanagerconfig -n ebpf-project rca-summarizer` 로 CRD 등록 확인.
- 2차 (fail-on-miss): Alertmanager API `/api/v2/status` 의 `configYAML` 응답에 4 분기 노드 (critical / capacity / anomaly / fallback) 가 모두 포함되어 있는지 정합 검증. kube-prometheus-stack reconcile 완료 까지 polling.
- 3차 (fail-on-miss): `amtool config routes test` 로 라벨 셋 별 dry-run 매칭. 4 분기 의도 (critical / capacity / anomaly / fallback) 가 정확한 receiver 로 라우팅 되는지 확인.

## 한계

- 실제 Slack / SMTP / PagerDuty 수신 검증은 외부 채널 부재로 본 가드 범위 외. 외부 채널 통합 시점에 별도 e2e 가드 추가 필요.
- amtool 은 `alertmanager-kube-prometheus-stack-alertmanager-0` Pod 안 의 `/bin/amtool` 을 kubectl exec 로 호출. helm release 이름 변경 시 `ALERTMGR_POD` env override.
- 본 가드는 routing tree 의 구조 정합과 매칭 의도만 검증. routing 의 group_interval / repeat_interval 운영 효과 (burst suppression) 의 실측 검증은 시간 기반 시나리오 필요로 본 가드 범위 외.
