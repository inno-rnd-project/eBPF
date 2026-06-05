# Prometheus retention 디스크 용량 자동 모니터링 (#108)

본 문서는 `deploy/monitoring/prometheus-pv-alert.yaml` 의 4 alert와 1 recording rule의 운영 의미, retention 60일 상향 (#88) 의 디스크 사용량 추정, PV resize와 retention 축소 fallback 절차, kubelet metrics 미수집 환경의 대응 패턴, troubleshooting 5 케이스를 정리한다.

## retention 60일 상향의 디스크 사용량 추정

#88 의 retention 상향 (`prometheus.spec.retention=60d`) 이전의 기본값은 10일 이었다. retention 시간이 6배 늘면 head 와 head_chunks 외 의 block 데이터 누적량도 약 6배 비례한다. dev cluster 의 실측 기준 추정 식은 다음과 같다.

- **10일 기준 사용량**: `prometheus_tsdb_storage_blocks_bytes` 직전 평균 (dev cluster 실측 ~5.2 GB)
- **60일 추정 사용량**: 10일 기준 × 6 = ~31 GB (head + WAL 추가 분 ~1-2 GB 별도)
- **dev cluster 호스트 root fs 현재**: 495 GB 총 용량 중 343 GB 사용 (69%). Prometheus 외 워크로드 (gpuobs / netobs / injector 등) 데이터까지 포함된 값. retention 60일 누적은 약 25 GB 추가 예상

본 PR의 alert 임계값 (80% / 90%) 은 위 추정 사용량 대비 95 GB / 50 GB 추가 마진을 제공한다. retention 60일이 충분히 누적될 때까지 alert false positive는 발생하지 않을 것으로 추정된다.

## 4 alert의 운영 의미와 대응 절차

### PrometheusVolumeUsageHigh (warning, `> 80%` for 15m)

- **트리거 조건**: `prometheus:host_disk_usage_ratio:5m > 0.8` 15분 지속
- **운영 의미**: Prometheus PV (또는 hostPath fallback의 host root fs) 사용률이 정상 운영 범위를 벗어났다. 즉시 대응은 불요하지만 6-12시간 이내 PV resize 또는 retention 축소 계획 수립 필요
- **routing**: `component=prometheus-capacity` 라벨로 #106 AlertmanagerConfig의 capacity 분기 (`groupInterval=5m`, `repeatInterval=12h`) 흡수
- **대응 절차**: `kubectl get prometheus -n monitoring -o jsonpath='{.spec.retention}'` 로 현재 retention 확인 후 PV resize 절차 또는 retention 축소 fallback 진행

### PrometheusVolumeUsageCritical (critical, `> 90%` for 5m)

- **트리거 조건**: `prometheus:host_disk_usage_ratio:5m > 0.9` 5분 지속
- **운영 의미**: Prometheus가 곧 TSDB block write 실패 또는 WAL 손상에 진입한다. 즉시 대응 필요
- **routing**: `component=prometheus` 라벨로 #106의 critical 분기 (`groupInterval=30s`, `repeatInterval=1h`) 흡수
- **대응 절차**: 즉시 retention 축소 `kubectl patch prometheus -n monitoring kube-prometheus-stack-prometheus --type=merge -p '{"spec":{"retention":"30d"}}'` 적용 후 회복 확인. 그 다음 PV resize 계획 수립

### PrometheusHighCardinality (warning, `head_series > 2M` for 30m)

- **트리거 조건**: `prometheus_tsdb_head_series > 2000000` 30분 지속
- **운영 의미**: 라벨 카디널리티가 정상 운영 범위를 벗어났다. retention 60일 상향 후 신규 메트릭 도입 또는 라벨 셋 누수 의심
- **routing**: `component=prometheus-capacity` 라벨로 capacity 분기 흡수
- **대응 절차**: `topk(10, count by(__name__)({__name__=~".+"}))` 로 cardinality 상위 10 메트릭 식별. 신규 도입 메트릭의 라벨 셋이 unbounded인지 확인. relabel_config drop 또는 recording rule aggregation 검토

### PrometheusMemoryPressure (warning, `RSS > 3 GB` for 15m)

- **트리거 조건**: `process_resident_memory_bytes{job=~".*prometheus.*"} > 3000000000` 15분 지속
- **운영 의미**: Prometheus process memory가 OOM 위험 범위에 진입. dev cluster 환경의 `container_spec_memory_limit_bytes` 메트릭 부재로 절대값 임계 사용
- **routing**: `component=prometheus-capacity` 라벨로 capacity 분기 흡수
- **대응 절차**: PrometheusHighCardinality 와 cross-reference. cardinality가 정상이면 retention 또는 query load 검토. cardinality도 spike이면 본 alert과 함께 발화하므로 동일 root cause 추적

## PV resize 절차

dev cluster의 hostPath / emptyDir 환경에서는 PV resize가 불가능하므로 retention 축소가 유일한 대안. 향후 PVC 환경으로 전환 시의 PV resize 절차는 다음과 같다.

```sh
# 1. StorageClass의 allowVolumeExpansion 확인
kubectl get storageclass -o jsonpath='{.items[*].allowVolumeExpansion}'

# 2. PVC의 spec.resources.requests.storage 갱신
kubectl patch pvc -n monitoring prometheus-kube-prometheus-stack-prometheus-db-prometheus-kube-prometheus-stack-prometheus-0 \
  --type=merge -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'

# 3. CSI 의 ResizeRequired 단계 진행 확인
kubectl describe pvc -n monitoring prometheus-...

# 4. file system resize 가 자동 진행되지 않 으면 prometheus pod 재기동
kubectl delete pod -n monitoring prometheus-kube-prometheus-stack-prometheus-0
```

`allowVolumeExpansion=false` 인 StorageClass라면 PVC를 새 size로 재생성 후 데이터 마이그레이션 필요.

## retention 축소 fallback 절차

```sh
# 1. 현재 retention 확인
kubectl get prometheus -n monitoring kube-prometheus-stack-prometheus -o jsonpath='{.spec.retention}'

# 2. 임시 retention 축소 (60d -> 30d)
kubectl patch prometheus -n monitoring kube-prometheus-stack-prometheus \
  --type=merge -p '{"spec":{"retention":"30d"}}'

# 3. prometheus pod 자동 rolling restart 대기
kubectl rollout status statefulset/prometheus-kube-prometheus-stack-prometheus -n monitoring

# 4. 디스크 사용률 회복 확인 (60d 데이터 정리에 ~30분 소요)
PROM_IP=$(kubectl get svc -n monitoring kube-prometheus-stack-prometheus -o jsonpath='{.spec.clusterIP}')
curl -sf -G "http://${PROM_IP}:9090/api/v1/query" --data-urlencode 'query=prometheus:host_disk_usage_ratio:5m'

# 5. 회복 후 deploy/monitoring/prometheus-retention-patch.yaml 의 retention 값을 영구 정정
```

축소 후 #88 의 capacity-trends 패널 데이터 cover 범위가 30일로 축소되어 분기 단위 분석에 영향. 재상향은 PV resize 또는 disk 확보 후 진행한다.

## kubelet metrics 미수집 환경의 fallback 매칭

dev cluster의 Prometheus가 PVC 부재 (hostPath / emptyDir) 환경이라 `kubelet_volume_stats_used_bytes` / `kubelet_volume_stats_capacity_bytes` 메트릭이 emit되지 않는다. 본 PR의 recording rule은 node-exporter의 `node_filesystem_*` 메트릭으로 fallback해 root filesystem 사용률을 Prometheus PV 사용률의 proxy로 활용한다.

향후 PVC 환경으로 전환 시 정정 패턴:

```yaml
- record: prometheus:volume_usage_ratio:5m
  expr: |
    avg_over_time(
      (
        kubelet_volume_stats_used_bytes{persistentvolumeclaim=~"prometheus-.*"}
        / kubelet_volume_stats_capacity_bytes{persistentvolumeclaim=~"prometheus-.*"}
      )[5m:]
    )
```

PVC 환경에서 `kubelet_volume_stats_*` 메트릭이 정상 emit되는지 확인 후 위 expr로 정정. node-exporter fallback은 root fs를 cover하므로 PVC 환경에서는 정확한 attribution이 불가능.

## Troubleshooting

- **PV 사용량 metric 부재**: `count(kubelet_volume_stats_used_bytes)` 가 0 이면 PVC 부재 환경. 본 PR의 node-exporter fallback이 활성인지 `prometheus:host_disk_usage_ratio:5m` 시리즈로 확인. node-exporter도 부재면 dev cluster의 monitoring stack 자체가 미설치 상태
- **cardinality spike 원인 추적**: `topk(10, count by(__name__)({__name__=~".+"}))` 로 cardinality 상위 10 메트릭 식별 후 신규 도입 메트릭의 라벨 셋 검토. `prometheus_tsdb_head_chunks` 와 cross-reference로 storage 부담 평가
- **OOM 발생 후 recovery**: Prometheus pod가 OOM으로 재기동되면 WAL replay에 수 분 소요. `kubectl logs -n monitoring prometheus-kube-prometheus-stack-prometheus-0 -c prometheus` 로 replay 진행 확인. 재기동 후 retention 축소 또는 PV resize로 root cause 제거
- **node-exporter fallback 의 attribution 부정확**: 본 fallback이 Prometheus 외 워크로드의 디스크 사용량도 함께 cover하므로 alert 발화 시 Prometheus가 root cause인지 확인 필요. `prometheus_tsdb_storage_blocks_bytes` 추세와 host disk 사용률 추세 cross-reference로 attribution 판단
- **alert 발화 후 routing 미동작**: #106 AlertmanagerConfig의 routing tree 정합 확인. `kubectl get alertmanagerconfig -n ebpf-project rca-summarizer` 와 Alertmanager `/api/v2/status` 의 configYAML 응답에서 component 라벨 매처 정상 등록 확인

## 비목표 (별도 이슈 위임)

- 자동 PV resize controller: CSI의 `allowVolumeExpansion` 활용한 자동화는 본 이슈 외 인프라
- retention 정책의 동적 조정 (운영 중 retention 값 자동 하향): 본 이슈 외
- Alertmanager / Grafana / Loki 의 별도 PV 사용량 alert: 본 PR 외. 본 alert 패턴 (`prometheus:host_disk_usage_ratio:5m` 또는 PVC 환경의 `kubelet_volume_stats_*`) 재사용 가능
- recording rule evaluation duration alert: retention 상향으로 인한 1시간 윈도우 rule의 eval 시간 증가는 별도 이슈
- remote write backlog alert: future federation / remote write 도입 시 별도
- Prometheus TSDB의 WAL fragmentation 또는 compaction 부담 모니터링: 본 이슈 외
