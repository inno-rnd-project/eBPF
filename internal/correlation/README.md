# internal/correlation

본 패키지는 Prometheus query_range API로 가져온 netobs와 gpuobs 시계열의 Pearson 상관계수를 산출하는 stateless 라이브러리다. 데이터 수집 파이프라인 (DaemonSet agent → Prometheus scrape → TSDB) 과 분리된 후행 분석 layer로 동작한다. 두 가지 표면이 본 라이브러리를 호출한다. 운영자의 1회성 진단은 `cmd/correlation-debug` CLI 가, 주기적 자동화와 Prometheus 메트릭 노출은 `cmd/correlation-exporter` Deployment 가 담당한다.

## 책임

본 패키지가 다루는 책임은 다음 네 가지다.

- Prometheus `/api/v1/query_range`로 다중 메트릭의 시계열을 가져오기 (`fetcher.go`)
- 노드 내 Pod 페어를 enumerate해 cross-product 폭발을 통제 (`pair.go`)
- 각 페어의 Pearson 상관계수를 lag 0 / +1 / -1 세 시점에서 산출 후 최대 절대값 채택 (`pearson.go`)
- 결과를 `CorrelationResult` slice로 반환 (`correlator.go`)

모든 산출은 호출 단위 stateless다. 시계열 buffer는 함수 scope 내 임시 자료로만 존재하고 GC된다. 영구 저장소를 두지 않으며 cluster에 새 워크로드를 추가하지 않는다.

## 기본 입력 메트릭

`DefaultConfig`가 zero-config 호출에서 사용하는 7개 query는 다음과 같다. 본 시리즈의 #47 / #48 / #49에서 도입된 recording rule을 직접 참조하며 모두 (node, src_namespace, src_pod, src_pod_uid) 라벨을 보유해 EnumeratePairs 의 Pod 페어 schema 와 정합한다.

- pod 단위 cause score 5종: `pod:cpu_throttle_score:5m`, `pod:memory_pressure_score:5m`, `pod:network_throughput_score:5m`, `pod:network_retrans_score:5m`, `pod:host_compute_stall_score:5m`
- pod 단위 GPU 메모리 비율 (gpu_uuid / gpu_index 라벨을 pod 단위로 집계해 normalize): `avg by(node, src_namespace, src_pod, src_pod_uid) (pod:gpu_memory_utilization_ratio:5m)`
- pod 단위 latency p99: `histogram_quantile(0.99, sum by(node, src_namespace, src_pod, src_pod_uid, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))`

node-level 메트릭 (`node:gpu_idle:5m` 등) 은 namespace / pod 라벨이 없어 본 패키지의 Pod 페어 schema 와 불일치라 default 에서 제외된다. 운영자가 명시적으로 node × pod 분석이 필요하면 별도 도구 또는 future cross-level schema (follow-up) 를 사용한다.

운영자는 `-extra-metric` flag로 추가 query를 등록 가능하다. 본 7종이 cluster의 PrometheusRule (`deploy/gpuobs/base/prometheus-rule.yaml`) 에 deploy되어 있어야 fetcher가 데이터를 받는다.

## CLI 사용

### 빌드

```sh
make build-correlation-debug
```

산출물은 `./bin/correlation-debug`다.

### Prometheus 접근 준비

cluster 외부에서 실행하려면 Prometheus 서비스에 접근 경로가 필요하다. 두 가지 방법 중 하나를 사용한다.

- 포트 포워딩: `kubectl port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9090:9090`
- ClusterIP 직접 접근 (운영자가 cluster 네트워크에서 실행 시): Prometheus svc IP를 `-prometheus-url`로 전달

### 실행 예시

```sh
# 직전 1시간 윈도우 산출
./bin/correlation-debug -prometheus-url http://localhost:9090 -window 1h

# 24시간 윈도우 + 추가 metric 등록
./bin/correlation-debug \
  -prometheus-url http://localhost:9090 \
  -window 24h \
  -min-samples 60 \
  -extra-metric 'rate(container_cpu_usage_seconds_total[5m])'

# Top-N 강한 상관 추출 (jq 후처리)
./bin/correlation-debug -window 1h | \
  jq '[.[] | select(.status == "ok")] | sort_by(-.max_abs_value) | .[:10]'
```

### 환경 변수

`PROMETHEUS_URL` env가 `-prometheus-url` flag보다 우선순위 낮은 default로 적용된다.

## 출력 schema

CLI는 `[]CorrelationResult`를 indented JSON으로 stdout에 emit한다. 각 element 구조는 다음과 같다.

```json
{
  "pair": {
    "src_namespace": "gpu-monitoring",
    "src_pod": "dcgm-dcgm-exporter-85bp8",
    "src_metric": "pod:cpu_throttle_score:5m",
    "dst_namespace": "ebpf-project",
    "dst_pod": "netobs-agent-5trsf",
    "dst_metric": "pod:memory_pressure_score:5m"
  },
  "correlation_by_lag": {
    "-1": -0.0962,
    "0":  -0.1598,
    "1":  -0.1808
  },
  "max_abs_lag": 1,
  "max_abs_value": 0.1808,
  "sample_count": 121,
  "status": "ok"
}
```

필드 의미는 다음과 같다.

- `pair`: 페어 정체성 8필드 (`src_namespace`, `src_pod`, `src_pod_uid`, `src_metric`, `dst_namespace`, `dst_pod`, `dst_pod_uid`, `dst_metric`). `src_pod_uid` / `dst_pod_uid` 는 입력 recording rule 이 본 라벨을 보존할 때만 채워진다. 본 시리즈 #49 의 cause score 는 cAdvisor `pod` / `namespace` 라벨에서 alias 만 만들어 UID 가 없으니 빈 문자열로 emit 된다. UID 보강은 별도 PrometheusRule 작업으로 분리
- `correlation_by_lag`: lag step별 Pearson 산출값 (-1 에서 1 사이)
- `max_abs_lag`: 최대 절대값을 보인 lag step
- `max_abs_value`: 최대 절대값 그 자체 (운영자가 "강한 상관"으로 거를 때 쓰는 일차 지표)
- `sample_count`: NaN/Inf pairwise 제거 후 유효 표본 수
- `status`: 산출 분류 (`ok` / `partial` / `skipped_low_samples` / `skipped_constant`)

`status`별 의미는 `types.go`의 const 주석을 참고한다.

## 정책

본 라이브러리가 고정한 정책은 다음과 같다.

- **lag 의미**: cross-correlation 관례를 따라 lag k가 양수면 `corr(a[t], b[t+k])`를 계산 (a가 b를 k step 앞서는 propagation)
- **pair enumeration**: 동일 node 라벨 + self 제외 + (X,Y) (Y,X) 양방향. cross-node pair는 본 패키지 범위 밖
- **NaN/Inf 처리**: pairwise 제거 후 산출. NaN이 결과로 새 나가지 않게 가드
- **length mismatch**: 짧은 쪽 기준 truncate
- **분산 0 (상수 시계열)**: `0.0`과 `StatusSkippedConstant` 반환. Pearson 정의상 분모 0이 되는 NaN을 명시적 fallback
- **표본 부족**: `MinSamples` 미만이면 `StatusSkippedLowSamples`. default 60 (1h / 30s 윈도우의 절반 이상)

## 실 클러스터 24h 검증 결과

본 PR 검증 시점 (dev cluster) 의 24시간 윈도우 산출 결과는 다음과 같다.

- 총 산출 페어: 808
- `status=ok`: 672건
- `status=skipped_constant`: 136건 (트래픽 없는 dev 상태의 일부 score가 sustained 0)
- NaN 출현: **0건** (수용 조건 충족)
- `|max_abs_value| > 1`: **0건** (수용 조건 충족)
- `max_abs_value` 최대: 0.7584
- `max_abs_value` 중앙값: 0.0396

수용 조건 "실 클러스터의 24h 데이터로 호출되어 합리적 (NaN 없음, 절대값 1 이하) score를 반환" 충족.

## 합성 시나리오 cross-validation

`#49` docs의 합성 시나리오 (CPU stress, iperf3 네트워크 부하) 와 본 패키지 산출 결과의 일치성 검증은 다음 절차로 수행한다.

- dev cluster에 stress-ng / iperf3 workload Pod을 5분 이상 가동
- `kubectl port-forward`로 Prometheus 접근
- `./bin/correlation-debug -window 10m -min-samples 10` 실행
- 결과에서 부하 Pod의 `pod:cpu_throttle_score:5m` 또는 `pod:network_throughput_score:5m`과 인접 Pod의 latency 사이 `max_abs_value`가 0.5 이상으로 잡히는지 확인

본 절차는 `#52` (injector) 머지 후 자동화로 흡수 예정이다.

## limitations

- **lag 범위**: 기본 ±1 step (30s)만 산출. 더 긴 propagation delay는 `-lag-steps` flag로 확장 (예: `-3,-2,-1,0,1,2,3`)
- **same-node 한정**: cross-node pair는 분석 의미가 모호하고 cardinality 위험이 커 본 PR에서 제외. 별도 follow-up에서 다룸
- **N^2 cost**: 노드당 Pod 수가 많을수록 페어 수가 2차 증가. 50 Pod 노드에서 50 × 50 × 11 metric × 3 lag = 약 82k 산출. 1회성 CLI 실행은 수 초 내 완료되나 exporter 주기 실행 시 부담
- **Prometheus 인증**: kube-prometheus-stack default unauth in-cluster 가정. mTLS / OIDC 환경은 reverse proxy로 우회 필요
- **단순 max 결합**: `pod:host_compute_stall_score:5m`의 max 결합 한계는 `#49`와 동일하게 follow-up에서 정밀화

## correlation-exporter

`cmd/correlation-exporter` 는 본 라이브러리를 reuse 하는 long-running Deployment 다. `RECONCILE_INTERVAL` cadence 로 `Correlate(ctx, time.Now())` 를 호출하고 `SelectTopN` 으로 Top-N noisy neighbor 페어를 추출해 두 Prometheus gauge 로 노출한다.

### 설치

dev 클러스터는 로컬 빌드 이미지를 사용한다.

```sh
make build-correlation-exporter
make image-build-correlation-exporter
make deploy-correlation-dev
```

prod 클러스터는 ghcr 의 정식 이미지를 가져온다.

```sh
make image-push-correlation-exporter
make deploy-correlation-prod
```

### 노출 메트릭

`correlation-exporter` 가 `/metrics` 에서 emit 하는 series 는 다음과 같다.

- `correlation_noisy_neighbor_score{victim_namespace, victim_pod, victim_pod_uid, suspect_namespace, suspect_pod, suspect_pod_uid, resource_dimension, rank}`: Pearson 상관계수 최대 절대값. 0 ~ 1 사이.
- `correlation_noisy_neighbor_lag_seconds{...동일 라벨...}`: 최대 절대값이 잡힌 lag 의 초 단위 환산. 양수면 suspect 가 victim latency 를 선행하는 인과 방향.
- `correlation_reconcile_duration_seconds`: 마지막 reconcile cycle 의 walltime.
- `correlation_reconcile_pairs_total`: `Correlate` 산출 페어의 누적 합계.
- `correlation_reconcile_neighbors_total`: `SelectTopN` 채택 후 emit 된 noisy neighbor 엔트리의 누적 합계.
- `correlation_reconcile_skipped_total{reason}`: skip 된 페어의 누적 합계. reason 은 `low_samples` 또는 `constant`.
- `correlation_reconcile_last_success_timestamp_seconds`: 마지막 성공 reconcile 의 epoch.
- `correlation_reconcile_errors_total`: 누적 reconcile 에러 수.

`resource_dimension` 라벨 값은 `cpu`, `memory`, `network`, `gpu` 네 가지다. 이슈 원안의 `disk_io` 는 본 시리즈가 disk 차원 cause score 를 도입하지 않아 제외되며 latency 는 dimension 이 아니라 victim 식별 기준이므로 라벨 값으로 두지 않는다. `rank` 라벨은 1 부터 `TOP_N` (기본 10) 까지로 제한된다.

### 카디널리티 분석

victim 1000 pod × dimension 4 × rank 10 = 40,000 series 가 noisy_neighbor 메트릭의 상한이다. self-health 메트릭 7 종을 더해도 40,007 series 로 단일 Prometheus 인스턴스가 처리하기에 안전한 규모다. `TOP_N` 의 binary-side 상한은 100 으로 가드되어 운영자가 의도치 않게 series 폭주를 일으킬 수 없다.

### alert runbook

#### CorrelationStrongNoisyNeighbor

특정 victim Pod 의 latency 가 한 suspect Pod 의 자원 변동과 max |corr| 0.85 이상으로 10 분 지속 동조한 상태다. 다음 순서로 대응한다.

- alert label 의 `victim_namespace/victim_pod` 와 `suspect_namespace/suspect_pod`, `resource_dimension` 을 확인
- Grafana `correlation-overview` 대시보드에서 두 라벨로 drill down 해 lag_seconds 방향 (양수면 suspect 가 victim 을 선행) 을 점검
- 원본 시계열은 `kubectl port-forward` 로 Prometheus 에 접근해 `pod:<dimension>_score:5m{src_pod="<suspect>"}` 와 victim latency p99 를 같은 1 시간 윈도우로 비교
- 본 패키지의 1 회성 재현은 `./bin/correlation-debug -window 1h -extra-metric 'pod:<dimension>_score:5m{src_pod="<suspect>"}'` 형태로 수행

#### CorrelationExporterStalled

마지막 reconcile 성공이 10 분 이상 지났다. 다음을 점검한다.

- `kubectl -n ebpf-project get pods -l app.kubernetes.io/name=correlation-exporter` 로 Pod 상태와 restart count 확인
- Pod 가 정상이면 `kubectl logs` 로 reconcile error 메시지 확인 (Prometheus 응답 코드, query 문법, timeout)
- Prometheus 서비스 도달성 확인. ConfigMap 의 `PROMETHEUS_URL` 이 cluster 내 도달 가능한 svc 를 가리키는지 점검

#### CorrelationExporterReconcileErrors

reconcile cycle 이 10 분 윈도우에서 3 회 이상 에러로 종료됐다.

- `kubectl logs` 로 에러 메시지 확인
- Prometheus 응답이 5xx 면 cluster Prometheus 의 load 와 query 성능 점검
- query 문법 에러면 `DefaultMetrics` 의 recording rule 이 변경됐는지 확인

### 운영자 drill-down 절차

`CorrelationStrongNoisyNeighbor` alert 을 받았을 때 정확히 같은 결과를 CLI 에서 재현하는 방법은 다음과 같다.

```sh
kubectl port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9090:9090 &
./bin/correlation-debug -prometheus-url http://localhost:9090 -window 1h | \
  jq '[.[] | select(.status == "ok" and (.pair.dst_metric | test("latency"))) ] |
      sort_by(-.max_abs_value) | .[:10]'
```

본 결과의 상위 페어가 alert label 의 (victim, suspect) 와 일치해야 한다. 일치하지 않으면 exporter 의 `RECONCILE_INTERVAL` 보다 짧은 시간에 상관 관계가 사라졌거나 (Pod 재시작 등) `WINDOW` 차이로 인한 noise 가 의심된다.
