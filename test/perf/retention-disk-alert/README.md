# retention-disk-alert e2e 회귀 가드 (#108)

이슈 #108의 Prometheus retention 디스크와 OOM 위험 alert 도입의 회귀 가드 스크립트다. alert rule 5종 (4 alert + 1 recording rule) 의 prometheus-operator reconcile 후 등록 정합과 recording rule의 실 산출을 자동 검증한다. 실제 발화 시뮬은 PV 직접 spike (dd / fallocate) 가 Prometheus TSDB 동작에 영향 가능하므로 본 verify에서는 채택하지 않고 임계값 임시 하향 패치 절차를 docs로 안내한다.

## 실행

```sh
test/perf/retention-disk-alert/verify.sh
```

env로 override 가능.

- `RULE_TIMEOUT` (기본 180s): rule 등록 polling timeout
- `PROM_URL` (미설정 시 자동 구성): Prometheus base URL을 직접 주입. `kubectl port-forward` 사용 시 `PROM_URL=http://localhost:9090` 형태로 지정
- `PROM_NAMESPACE` (기본 `monitoring`): Prometheus 리소스의 namespace. `PROM_URL` 미설정 시 ClusterIP 조회에 사용
- `PROM_SVC` (기본 `kube-prometheus-stack-prometheus`): Prometheus Service 이름
- `PROM_PORT` (기본 `9090`): Prometheus API 포트

`PROM_URL`이 미설정이면 스크립트가 자동으로 ClusterIP를 조회하고 ClusterIP가 비거나 `"None"` (Headless Service) 인 경우 in-cluster DNS (`${PROM_SVC}.${PROM_NAMESPACE}.svc.cluster.local`) 로 fallback한다. 클러스터 내부 / 외부 / port-forward / Headless Service 모든 환경에서 동작 가능.

## 가드 단계

- **1차 (fail-on-miss)**: `prometheus:host_disk_usage_ratio:5m` recording rule과 `PrometheusVolumeUsageHigh`, `PrometheusVolumeUsageCritical`, `PrometheusHighCardinality`, `PrometheusMemoryPressure` 4 alert가 Prometheus `/api/v1/rules` 응답에 등록되어 있는지 확인. prometheus-operator reconcile이 완료될 때까지 polling
- **1.5차 (warn-only)**: recording rule의 실 산출 확인. node-exporter의 `node_filesystem_*` 메트릭이 정상 emit되는 환경에서만 산출 가능. eval interval 30s 경과 후 결과
- **2차 (manual / warn-only)**: 실제 발화 검증은 임계값 임시 하향 패치 절차로 docs 안내. 본 verify에서는 직접 시뮬하지 않고 운영자가 수동 검증 가능한 절차만 출력

## 한계

- 본 alert의 임계값은 dev cluster의 hostPath / emptyDir 환경 (PVC 부재) 에서 node-exporter `node_filesystem_*` fallback을 기준으로 설계되었다. PVC 환경으로 전환 시 `kubelet_volume_stats_*` 기반 expr로 정정 필요
- 실제 PV 사용량 spike 시뮬은 Prometheus TSDB 동작에 영향 가능하므로 본 verify 범위 외. 임계값 임시 하향 패치 절차가 안전한 대안
- `PrometheusHighCardinality` (head_series > 2M) 와 `PrometheusMemoryPressure` (RSS > 3 GB) 의 실제 발화 시뮬은 합성 메트릭 폭증이 필요해 본 verify 범위 외
- node-exporter 가 미설치 또는 일부 노드에만 떠 있는 환경에서는 1.5차 가드가 warn 처리. 본 시점의 dev cluster는 1 노드의 node-exporter 만 정상 emit
