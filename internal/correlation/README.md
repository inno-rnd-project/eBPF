# internal/correlation

본 패키지는 Prometheus query_range API로 가져온 netobs와 gpuobs 시계열의 Pearson 상관계수를 산출하는 stateless 라이브러리다. 데이터 수집 파이프라인 (DaemonSet agent → Prometheus scrape → TSDB) 과 분리된 후행 분석 layer로 동작하며 운영자가 `cmd/correlation-debug` CLI로 1회성 호출하는 형태가 이슈 #50의 expose 범위다. 주기적 자동화 (exporter / CronJob) 는 #51에서 다룬다.

## 책임

본 패키지가 다루는 책임은 다음 네 가지다.

- Prometheus `/api/v1/query_range`로 다중 메트릭의 시계열을 가져오기 (`fetcher.go`)
- 노드 내 Pod 페어를 enumerate해 cross-product 폭발을 통제 (`pair.go`)
- 각 페어의 Pearson 상관계수를 lag 0 / +1 / -1 세 시점에서 산출 후 최대 절대값 채택 (`pearson.go`)
- 결과를 `CorrelationResult` slice로 반환 (`correlator.go`)

모든 산출은 호출 단위 stateless다. 시계열 buffer는 함수 scope 내 임시 자료로만 존재하고 GC된다. 영구 저장소를 두지 않으며 cluster에 새 워크로드를 추가하지 않는다.

## 기본 입력 메트릭

`DefaultConfig`가 zero-config 호출에서 사용하는 11개 query는 다음과 같다. 본 시리즈의 #47 / #48 / #49에서 도입된 recording rule을 직접 참조한다.

- pod 단위 cause score 6종: `pod:cpu_throttle_score:5m`, `pod:memory_pressure_score:5m`, `pod:network_throughput_score:5m`, `pod:network_retrans_score:5m`, `pod:host_compute_stall_score:5m`, `pod:gpu_memory_utilization_ratio:5m`
- pod 단위 latency: `histogram_quantile(0.99, sum by(...)(rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))`
- node 단위 자원 4종: `node:gpu_idle:5m`, `node:gpu_pcie_saturation_score:5m`, `node:gpu_util_p95:5m`, `node:gpu_throttle_seconds:rate5m`

운영자는 `-extra-metric` flag로 추가 query를 등록 가능하다. 본 11종이 cluster의 PrometheusRule (`deploy/gpuobs/base/prometheus-rule.yaml`) 에 deploy되어 있어야 fetcher가 데이터를 받는다.

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

- `pair`: 페어 정체성 6필드 (`src_namespace`, `src_pod`, `src_metric`, `dst_namespace`, `dst_pod`, `dst_metric`)
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

## `#51` exporter 연계

본 CLI의 stdout JSON schema (`CorrelationResult`) 는 `#51` (Top-N noisy neighbor exporter) 의 입력으로 재사용된다. struct의 `json` tag가 명시적으로 부여되어 schema가 동결되어 있다. exporter는 본 패키지를 라이브러리로 import해 주기적 `Correlate(ctx)` 호출 후 결과를 Prometheus 메트릭으로 변환하는 형태를 따른다.
