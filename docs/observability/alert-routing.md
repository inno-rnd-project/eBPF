# Alert routing skeleton 운영 가이드 (#106)

본 문서는 `deploy/rca-summarizer/base/alertmanagerconfig.yaml` 의 4 분기 routing tree skeleton 의 구조와 운영 의도, 그리고 외부 채널 통합 시 본 skeleton 위에 receiver 를 추가 하는 후속 절차를 정리한다. 본 PR 시점에는 Slack / SMTP / PagerDuty 같은 외부 채널이 미통합 상태라 routing tree 의 4 분기 receiver 가 모두 `rca-summarizer` webhook 으로 동일 하며, skeleton 의 노드 분리 자체가 향후 채널 통합 시 receiver 만 교체 하면 즉시 활성화 되는 base 역할 을 한다.

## routing tree 4 분기 구조

본 PR 적용 후의 routing tree 는 다음 4 분기 구조 다.

- **(1) critical 분기** matcher `severity="critical"`. 모든 component 에 적용. `groupWait=5s`, `groupInterval=30s`, `repeatInterval=1h` 의 즉시 통보 정책. 향후 PagerDuty escalation 과 Slack `#oncall` 동시 송신 자리. 본 PR 시점 에는 `GPUIdleWithHostComputeStall` 1 종 alert 가 본 분기로 흡수된다.
- **(2) capacity 분기** matcher `severity="warning"` + `component=~".*-capacity"`. `groupWait=30s`, `groupInterval=5m`, `repeatInterval=12h` 의 저빈도 정책. capacity 선행 신호의 noise 를 누른다. 향후 Slack `#capacity-planning` 송신 자리. `gpuobs-capacity` / `netobs-capacity` / `cpu-capacity` / `memory-capacity` 의 #88 capacity-trends 4 종이 본 분기로 흡수된다.
- **(3) anomaly 분기** matcher `severity="warning"` + `component=~".*-anomaly"`. `groupWait=15s`, `groupInterval=30s`, `repeatInterval=4h` 의 중간 빈도 정책. spike 의 transient 특성을 반영. 향후 Slack `#oncall-secondary` 송신 자리. `gpuobs-anomaly` / `netobs-anomaly` / `cpu-anomaly` / `memory-anomaly` 의 #89 anomaly-spike 4 종이 본 분기로 흡수된다.
- **(4) fallback 분기** top-level route 자체. 위 3 분기 미매칭 alert (component 가 `gpuobs` / `netobs` / `correlation` / `observability` 인 일반 warning 과 라벨 부재 케이스) 모두 흡수. `groupWait=30s`, `groupInterval=5m`, `repeatInterval=24h` 의 최저 빈도 정책. 향후 Email 송신 자리.

## group_by 정책

모든 분기가 `[alertname, component, namespace]` 공통 유지로 cardinality 폭증을 차단한다. 동일 alert 가 multi-namespace 에서 발화해도 namespace 단위로 묶여 burst suppression 의도가 보존된다. namespace 라벨은 AlertmanagerConfig CRD 의 자동 부착 정책 (본 CRD 가 `namespace="ebpf-project"` matcher 를 자동 주입) 으로 보장된다.

## 외부 채널 통합 시 receiver 추가 절차

후속 follow-up 이슈에서 외부 채널이 통합되는 시점에 본 skeleton 위에 receiver 만 추가하면 4 분기가 즉시 활성화 된다. AlertmanagerConfig CRD 의 `receivers` 섹션에 다음 yaml snippet 을 추가하면 된다 (실제 값은 ConfigMap / Secret 으로 분리 주입).

### Slack incoming webhook 추가

```yaml
receivers:
  - name: slack-oncall
    slackConfigs:
      - apiURL:
          name: alertmanager-slack-secret
          key: oncall-webhook-url
        channel: "#oncall"
        sendResolved: true
        title: "[{{ .Status }}] {{ .GroupLabels.alertname }}"
        text: "{{ range .Alerts }}{{ .Annotations.description }}{{ end }}"
```

### SMTP email 추가

```yaml
receivers:
  - name: email-fallback
    emailConfigs:
      - to: oncall@example.com
        from: alertmanager@example.com
        smarthost: smtp.internal.example.com:587
        authUsername: alertmanager
        authPassword:
          name: alertmanager-smtp-secret
          key: password
        sendResolved: false
```

### PagerDuty integration 추가

```yaml
receivers:
  - name: pagerduty-critical
    pagerdutyConfigs:
      - routingKey:
          name: alertmanager-pagerduty-secret
          key: integration-key
        severity: critical
        description: "{{ .GroupLabels.alertname }} ({{ .GroupLabels.component }})"
```

receiver 추가 후 본 routing tree 의 각 분기 노드의 `receiver` 필드를 새 receiver 이름으로 교체하면 즉시 활성화된다. 본 PR 시점 의 `receiver: rca-summarizer` 가 `receiver: slack-oncall` 또는 `receiver: pagerduty-critical` 로 교체된다.

## kube-prometheus-stack Alertmanager 의 픽업 흐름

AlertmanagerConfig CRD 는 kube-prometheus-stack 의 Alertmanager Operator 가 매 reconcile 사이클마다 watch 후 alertmanager.yaml 의 routes 섹션에 자동 머지한다. CRD 의 namespace 매처 (자동 주입된 `namespace="ebpf-project"`) 가 사용되어 다른 namespace 의 alert 와 routing tree 가 격리된다. CRD 의 변경이 alertmanager.yaml 에 반영되는 시간은 통상 30-60 초 이내이며 `kubectl logs -n monitoring alertmanager-kube-prometheus-stack-alertmanager-0 -c alertmanager` 의 reload 로그로 확인 가능 하다.

## dev cluster 검증 절차

routing tree 정합은 외부 채널 없이 다음 3 단계로 확인 가능 하다.

### 1. CRD 등록 확인

```sh
kubectl get alertmanagerconfig -n ebpf-project rca-summarizer
```

### 2. Alertmanager API 의 configYAML 정합 확인

```sh
ALERTMGR_IP=$(kubectl get svc -n monitoring kube-prometheus-stack-alertmanager -o jsonpath='{.spec.clusterIP}')
curl -sf "http://${ALERTMGR_IP}:9093/api/v2/status" \
  | python3 -c 'import sys, json; print(json.load(sys.stdin)["config"]["original"])' \
  | grep -EA 20 'ebpf-project[/-]rca-summarizer[/-]rca-summarizer'
```

응답에 critical / capacity / anomaly 3 자식 노드와 4 종 matcher 가 포함되어야 한다. prometheus-operator 가 webhook URL 자체는 `configYAML` 응답 에서 마스킹 하므로 receiver 이름으로 매칭 한다. receiver 이름 포맷의 구분자 (slash vs hyphen) 는 prometheus-operator 버전에 따라 달라 character class `[/-]` 로 두 형식 모두 흡수 한다.

### 3. 합성 alert payload dry-run 매칭

```sh
amtool config routes test --config.file=<(curl -s "http://${ALERTMGR_IP}:9093/api/v2/status" | python3 -c "import sys,json; print(json.load(sys.stdin)['config']['original'])") \
  severity=critical component=correlation namespace=ebpf-project
```

라벨 셋 별 매칭 결과가 4 분기 의도와 일치하는지 확인. `test/perf/alert-routing/verify.sh` 가 본 3 단계를 자동화 한다.

## Troubleshooting

- alert 가 어느 분기에도 안 잡힘: `kubectl describe alertmanagerconfig -n ebpf-project rca-summarizer` 로 CRD 의 spec 적용 상태 확인. `kubectl logs -n monitoring alertmanager-kube-prometheus-stack-alertmanager-0 -c config-reloader` 로 reload 에러 확인.
- 동일 alert 가 top-level fallback 과 자식 분기 양쪽으로 send: 의도된 동작이다. top-level route 의 `continue: true` 와 각 자식 노드의 `continue: true` 가 결합 해 동일 alert 가 자식 분기 (예: critical) 매칭 후 top-level fallback 까지 도달 한다. 향후 외부 채널 통합 시 동일 alert 가 PagerDuty (critical 분기 receiver) 와 Email (fallback receiver) 양쪽 으로 동시 escalation 되는 구조의 base. 단 본 PR 시점 에는 모든 분기 receiver 가 동일 `rca-summarizer` 라 webhook 호출 횟수만 늘어나며 정합 문제는 없다. 자식 분기 끼리 (예: critical 과 capacity) 는 matcher 가 배타적 이라 동시 hit 가 발생 하지 않는다.
- repeatInterval 이 의도와 다름: AlertmanagerConfig CRD 의 한 사이클 reload 가 누락되었을 수 있음. 위 검증 2 의 configYAML 응답으로 실제 적용 값 확인.
- 외부 채널 미통합 상태에서 alert 가 Slack / Email / PagerDuty 로는 안 감: 본 PR 시점의 의도된 동작. 모든 alert 는 `rca-summarizer` webhook 에 정상 도달 하며 외부 SaaS 가시화 는 follow-up 이슈에서 receiver 추가 후 활성화 된다.
