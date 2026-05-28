# GPU x network cross-correlation e2e 검증 시나리오

이슈 #86 의 GPU-network cross-correlation 통합 패널이 dev cluster 에서 정상 emit 되는지 회귀 가드한다. dev cluster 전용이며 prod 에서는 실행하지 않는다.

## 사전 조건

- dev cluster 의 prometheus 가 `monitoring` namespace 에 ready
- `netobs-gpuobs-correlation` PrometheusRule 의 `netobs-gpuobs.cross-correlation.recording` group 이 적용된 후 최소 5 분 경과
- `gpuobs-agent` DaemonSet 이 ready 상태로 `gpuobs_device_*` 메트릭을 emit 중
- `observability-test` namespace 의 `client` Pod 와 `server` Pod 가 자연 트래픽을 주고 받는 중

## 시나리오 개요

- 1차 가드: 신규 recording rule 4 종이 동시에 non-empty 시리즈로 emit 되는지 확인 (namespace 필터 없음. dev cluster 의 netobs allowlist 설정에 따라 emit 가능 namespace 가 달라지므로 회귀 가드는 "어떤 namespace 든 산정 됨" 까지만 보장)
  - `count(node:gpu_util_ratio:5m) >= 1`
  - `count(node:gpu_memory_used_ratio:5m) >= 1`
  - `count(pod:network_throughput_bps:5m) >= 1`
  - `count(pod:network_p99_latency_seconds:5m) >= 1`
- 2차 가드 (warn only): correlation overlay 시리즈 가 present 한지 확인 하되 비어 있어도 hard fail 시키지 않는다. dev cluster idle 시 noisy_neighbor_score 가 emit 되지 않는 정상 동작 케이스를 차단하지 않기 위해 warn 으로만 노출

## 실행

```sh
./verify.sh
```

본 스크립트 는 별도 워크로드 spawn 없이 기존 observability-test namespace 의 자연 트래픽 만으로 최대 600 초 (30 초 간격 polling) 안에 4 가드 통과 여부를 확인한다.

## 종료 코드

- 0: 검증 통과
- 1: 검증 실패 (timeout, PrometheusRule 미적용, prometheus 미접근 등)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `GPUNET_NAMESPACE` | `ebpf-project` | PrometheusRule 의 namespace |
| `GPUNET_ALLOW_NAMESPACE` | `observability-test` | network 시계열의 src_namespace 라벨 값 |
| `GPUNET_TIMEOUT` | `600` | 통과 대기 timeout 초 (5 분 warmup 포함) |
| `GPUNET_POLL_INTERVAL` | `30` | prometheus polling 주기 초 |
| `GPUNET_PROM_NAMESPACE` | `monitoring` | prometheus Service namespace |
| `GPUNET_PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus Service 이름 |
| `GPUNET_PROM_PORT` | `9090` | prometheus Service port |

## 실패 시 진단

`[fail] timed out` 으로 떨어지면 다음 순서로 점검한다.

- `kubectl get prometheusrule -n ebpf-project netobs-gpuobs-correlation -o yaml` 로 `netobs-gpuobs.cross-correlation.recording` group 적용 여부 확인
- recording rule 적용 시점 부터 5 분 이상 경과 했는지 확인 (`kubectl get prometheusrule ... -o jsonpath='{.metadata.resourceVersion}'`)
- `gpuobs-agent` 의 Pod 상태와 `gpuobs_device_utilization_percent` 의 raw 시리즈 존재 확인
- `observability-test` namespace 의 `client` Pod 가 `Running` 상태이며 `server` Pod 로 트래픽을 발생시키는지 확인
- `netobs_pod_bytes_total{src_namespace="observability-test"}` 의 rate 가 0 보다 큰지 확인
