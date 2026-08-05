# Prometheus retention 디스크 용량 자동 모니터링 (#108)

본 문서는 `deploy/monitoring/prometheus-pv-alert.yaml` 의 6 alert와 1 recording rule의 운영 의미, retention 산정 근거와 디스크 사용량 추정, 스토리지 영속성 구성, retention 축소 fallback 절차, kubelet metrics 미수집 환경의 대응 패턴, troubleshooting 케이스를 정리한다.

## retention 산정 근거와 디스크 사용량 추정

retention 은 소비자가 요구하는 최장 참조 구간이 결정한다. 초기 도입(#88)은 z-score baseline 이 `[30d] offset 30d` 로 30~60일 전 구간을 참조해 60일이 필요했으나, #370 에서 trailing `[30d] offset 1h` 로 전환해 그 요구가 소멸했다. 현재 최장 참조는 다음 두 가지다.

- capacity-trends 계열 z-score 4종의 `[30d] offset 1h` (30일 1시간)
- overview 대시보드의 `now-4w` 패널 (28일)

따라서 최소 요구는 약 31일이고 `retention: 40d` 는 약 10일 여유를 남긴다. **31일 미만으로 내리면 capacity 판정이 성립하지 않으므로 축소 시 이 하한을 지켜야 한다.**

retention 을 조정할 때는 correlation API 의 `at` 파라미터 허용 범위 상수 (`internal/correlation/api/timeparam.go` 의 `atRetentionWindow`, 현재 45일) 도 함께 확인한다 (#411). 이 상수는 retention 보다 약간 넉넉하게 두어 정상 조회를 막지 않으면서 응답 캐시 키가 임의 과거 시점으로 증식하는 것을 차단하는 값이라, retention 을 크게 내리면 두 값의 간격이 벌어져 이미 조회 불가한 구간을 계속 허용하게 된다.

크기 상한은 `retentionSize: 40GB` 가 담당한다. local-path PV 는 노드 로컬 디렉터리라 PVC 요청 용량이 강제되지 않으므로 노드 디스크를 보호하는 실질 상한은 이 필드다. 실측 기준은 다음과 같다.

- **실측 유입률**: 블록 47.1 GiB 에 retention 60일이라 0.785 GiB/day. 이후 flow 계열 카디널리티 통제(#403)로 시리즈가 29.9만에서 18.9만으로 줄어 현재는 약 0.5 GiB/day
- **40일 추정 누적**: 약 20 GiB (head 와 WAL 별도)
- **40GB 상한의 의미**: 추정 대비 약 2배 여유. 크기 상한이 시간 상한보다 먼저 걸려 실질 retention 이 31일 하한을 깨지 않게 하는 값이며, 시리즈 규모가 크게 늘면 함께 재산정한다

## 스토리지 영속성 구성

TSDB 는 `deploy/monitoring/prometheus-storage-patch.yaml` 이 선언하는 local-path PVC(60Gi) 에 있고 gpu 노드에 고정된다.

- **노드 고정 이유**: master 와 worker 3대의 루트 디스크가 각 82 GiB 라 TSDB 를 수용하지 못한다. 전환 전 worker1 이 TSDB 47 GiB 때문에 84% 까지 포화했다. gpu 노드만 495 GiB 로 여유가 있다
- **트레이드오프**: gpu 노드는 본 프로젝트의 GPU 부하 테스트 대상이라 관측자가 관측 대상과 같은 노드에 상주한다. 부하 실험이 Prometheus 의 디스크와 CPU 에 영향을 줄 수 있다
- **한계**: local-path 는 hostPath 기반이라 pod 재시작과 노드 재부팅은 견디지만 노드 자체 소실은 견디지 못한다. `remote_write` 와 Thanos 가 없어 클러스터 외부 백업이 존재하지 않는다
- **PV reclaim policy**: `Retain` 으로 전환해 PVC 가 실수로 삭제돼도 PV 와 데이터가 남는다. StorageClass 가 volume expansion 을 지원하지 않아 용량 확장은 PVC 재생성이 필요하다
- **Retain 재적용**: 이 전환은 라이브 PV 에 대한 `kubectl patch` 였고 선언 어디에도 없다. local-path provisioner 가 새로 만드는 PV 는 다시 `Delete` 라, PVC 재생성 (용량 확장 절차 포함) 뒤에는 반드시 `kubectl patch pv <pv-name> -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'` 를 다시 적용한다

### emptyDir 구성에서의 이력 전량 소실 사고

전환 배경은 실제 사고다. 종전 스토리지가 chart 기본값인 emptyDir 였고 PVC 와 StorageClass 가 아예 없었는데, retention 변경을 위한 `kubectl patch` 가 operator 의 StatefulSet roll 을 유발해 최대 60일 이력 47 GiB 가 전량 삭제됐다. `remote_write` 도 Thanos 도 없어 복구가 불가능했다.

교훈은 두 가지다. **Prometheus CR 변경은 항상 pod roll 을 부르므로, 스토리지가 영속인지(`kubectl get pod -n monitoring prometheus-kube-prometheus-stack-prometheus-0 -o jsonpath='{.spec.volumes}'` 에서 emptyDir 여부) 먼저 확인해야 한다.** 그리고 이력 소실은 조용히 일어나므로 감지 장치가 필요하다. 과거 값 비교로는 원리적으로 감지할 수 없는데, 비교 대상이 되는 과거 값 자체가 소실과 함께 사라지기 때문이다. 그래서 `PrometheusStorageNotPersistent` 는 kube-state-metrics 의 PVC 존재라는 소실 독립 신호를 쓴다.

또한 이 patch 들은 Helm(kube-prometheus-stack) 이 관리하는 CR 을 kustomize 로 strategic merge 하는 구조다. Helm chart 기본값에는 `storage` 선언이 없으므로 **Helm 측 작업이 kustomize patch 를 덮으면 emptyDir 로 회귀한다.** `helm upgrade` 를 수행할 때는 반드시 `kubectl apply -k deploy/monitoring/` 를 다시 적용하고 PVC 가 유지되는지 확인해야 한다.

## 6 alert의 운영 의미와 대응 절차

### PrometheusVolumeUsageHigh (warning, `> 80%` for 15m)

- **트리거 조건**: `prometheus:host_disk_usage_ratio:5m > 0.8` 15분 지속
- **운영 의미**: Prometheus TSDB 가 있는 노드의 root fs 사용률이 정상 범위를 벗어났다. local-path PVC 사용량이 노드 root fs 에 누적되므로 Prometheus 증가와 무관 소비처(docker build cache 등)가 함께 반영되니 원인 귀속이 선행이다
- **routing**: `component=prometheus-capacity` 라벨로 #106 AlertmanagerConfig의 capacity 분기 (`groupInterval=5m`, `repeatInterval=12h`) 흡수
- **대응 절차**: `prometheus_tsdb_storage_blocks_bytes` 로 Prometheus 점유분을 먼저 확인한다. 무관 소비처가 지배하면 거기서 회수하고, Prometheus 가 지배하면 31일 하한을 지키며 retention 또는 retentionSize 를 조정한다

### PrometheusVolumeUsageCritical (critical, `> 90%` for 5m)

- **트리거 조건**: `prometheus:host_disk_usage_ratio:5m > 0.9` 5분 지속
- **운영 의미**: Prometheus가 곧 TSDB block write 실패 또는 WAL 손상에 진입한다. 즉시 대응 필요
- **routing**: `component=prometheus` 라벨로 #106의 critical 분기 (`groupInterval=30s`, `repeatInterval=1h`) 흡수
- **대응 절차**: Prometheus 점유분이 지배적인지 먼저 확인하고, 그렇다면 `retentionSize` 를 낮춰 즉시 블록을 정리한다. retention 시간을 줄일 때는 31일 하한을 지킨다. 무관 소비처가 원인이면 거기서 회수한다

### PrometheusHighCardinality (warning, `head_series > 2M` for 30m)

- **트리거 조건**: `prometheus_tsdb_head_series > 2000000` 30분 지속
- **운영 의미**: 라벨 카디널리티가 정상 운영 범위를 벗어났다. 신규 메트릭 도입 또는 라벨 셋 누수 의심
- **routing**: `component=prometheus-capacity` 라벨로 capacity 분기 흡수
- **대응 절차**: `topk(10, count by(__name__)({__name__=~".+"}))` 로 cardinality 상위 10 메트릭 식별. 신규 도입 메트릭의 라벨 셋이 unbounded인지 확인. relabel_config drop 또는 recording rule aggregation 검토

### PrometheusMemoryPressure (warning, `RSS > 3 GB` for 15m)

- **트리거 조건**: `process_resident_memory_bytes{job=~".*prometheus.*"} > 3000000000` 15분 지속
- **운영 의미**: Prometheus process memory가 OOM 위험 범위에 진입. dev cluster 환경의 `container_spec_memory_limit_bytes` 메트릭 부재로 절대값 임계 사용
- **routing**: `component=prometheus-capacity` 라벨로 capacity 분기 흡수
- **대응 절차**: PrometheusHighCardinality 와 cross-reference. cardinality가 정상이면 retention 또는 query load 검토. cardinality도 spike이면 본 alert과 함께 발화하므로 동일 root cause 추적

### PrometheusStorageNotPersistent (critical, PVC 부재 for 10m)

- **트리거 조건**: `absent(kube_persistentvolumeclaim_info{namespace="monitoring", persistentvolumeclaim=~"prometheus-.+-db-.+"})` 가 10분 지속하고 kube-state-metrics 가 정상인 경우
- **운영 의미**: 스토리지 선언이 유실되어 TSDB 가 ephemeral 상태다. 이 상태에서는 pod 재생성이 곧 전 이력 삭제이며 외부 백업이 없어 복구가 불가능하다. 가장 흔한 회귀 경로는 Helm 작업이 kustomize patch 를 덮는 경우다
- **routing**: `component=prometheus` 라벨로 critical 분기 흡수, 즉시 통보
- **설계 근거**: 이력 소실은 과거 값 비교로 감지할 수 없다. 비교 대상이 되는 과거 값 자체가 소실과 함께 사라지기 때문이다. 따라서 소실과 독립인 구조 신호(PVC 존재)를 쓰며, kube-state-metrics 장애 시의 `absent` 오발화를 막기 위해 `up` 게이트를 AND 로 둔다
- **대응 절차**: `kubectl get pvc -n monitoring` 로 확인하고 `kubectl apply -k deploy/monitoring/` 로 선언을 재적용한 뒤, pod 볼륨이 PVC 인지 `kubectl get pod -n monitoring prometheus-kube-prometheus-stack-prometheus-0 -o jsonpath='{.spec.volumes}'` 로 검증한다

### PrometheusHistoryInsufficient (warning, 이력 31일 미만 for 1h)

- **트리거 조건**: `(time() - prometheus_tsdb_lowest_timestamp_seconds) / 86400 < 31` 이 1시간 지속
- **운영 의미**: 장애 신호가 아니라 데이터 품질 신호다. 보존 이력이 30일 baseline 요구에 미달해 capacity-trends z-score 4종과 overview 4주 패널이 불완전하고, 그것을 소비하는 `*AnomalyDetected` alert 4종이 사실상 눈먼 상태임을 알린다
- **routing**: `component=prometheus-capacity` 라벨로 capacity 분기 흡수. 재축적 기간 내내 유지되는 성격이라 저빈도 통보(`repeatInterval=12h`) 가 적정하다
- **정상 운영에서는 미발화**: 이력이 retention(40일) 에 머물기 때문이다. 이력 소실이나 신규 구축 직후에만 발화한다
- **대응 절차**: 원인을 먼저 구분한다. `PrometheusStorageNotPersistent` 가 함께 발화하면 이력이 파괴된 것이고, 그렇지 않으면 신규 또는 최근 재구축된 TSDB 다. 후자라면 재축적을 기다리되 그 기간 capacity 이상 감지는 사용 불가로 취급한다

## PV resize 절차

현재는 local-path PVC 환경이나 StorageClass 가 volume expansion 을 지원하지 않아 in-place resize 가 불가능하다. 용량을 늘리려면 PVC 재생성이 필요하고 그 과정에서 이력이 끊기므로, 통상 대안은 retention 또는 retentionSize 축소다. expansion 을 지원하는 StorageClass 로 전환한 뒤의 resize 절차는 다음과 같다.

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

# 2. 임시 축소. 시간 하한 31일을 지켜야 하므로 크기 상한을 먼저 줄이는 편이 안전하다
kubectl patch prometheus -n monitoring kube-prometheus-stack-prometheus \
  --type=merge -p '{"spec":{"retentionSize":"25GB"}}'

# 3. prometheus pod 자동 rolling restart 대기
kubectl rollout status statefulset/prometheus-kube-prometheus-stack-prometheus -n monitoring

# 4. 디스크 사용률 회복 확인 (블록 정리에 ~30분 소요)
PROM_IP=$(kubectl get svc -n monitoring kube-prometheus-stack-prometheus -o jsonpath='{.spec.clusterIP}')
curl -sf -G "http://${PROM_IP}:9090/api/v1/query" --data-urlencode 'query=prometheus:host_disk_usage_ratio:5m'

# 5. 회복 후 deploy/monitoring/prometheus-retention-patch.yaml 의 retention 값을 영구 정정
```

축소는 임시 조치다. 시간 retention 을 31일 미만으로 내리면 capacity-trends baseline 이 성립하지 않으므로, 지속적으로 공간이 부족하면 디스크 확보나 카디널리티 감축으로 해결하고 값은 `deploy/monitoring/prometheus-retention-patch.yaml` 에 영구 반영한다.

## kubelet metrics 미수집 환경의 fallback 매칭

local-path PVC 로 전환한 뒤에도 이 클러스터는 `kubelet_volume_stats_used_bytes` 와 `kubelet_volume_stats_capacity_bytes` 를 수집하지 않는다. 그래서 recording rule 은 node-exporter 의 `node_filesystem_*` 로 fallback 해 root filesystem 사용률을 proxy 로 쓴다. local-path PV 의 실제 데이터가 노드 root fs 하위 디렉터리에 누적되므로 이 proxy 는 여전히 유효하다.

kubelet 메트릭이 수집되기 시작하면 다음 패턴으로 정정한다:

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

node-exporter fallback 은 노드 전체 root fs 를 재므로 Prometheus 점유분과 무관 소비처를 구분하지 못한다. 정확한 attribution 이 필요하면 `prometheus_tsdb_storage_blocks_bytes` 를 함께 본다.

## Troubleshooting

- **PV 사용량 metric 부재**: `count(kubelet_volume_stats_used_bytes)` 가 0 이면 kubelet volume stats 미수집 환경이다. PVC 유무와 별개이므로 PVC 상태는 `kubectl get pvc -n monitoring` 로 직접 확인하고, node-exporter fallback 활성은 `prometheus:host_disk_usage_ratio:5m` 시리즈로 확인한다
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
