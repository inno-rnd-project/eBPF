# flow tracking e2e 검증 시나리오

이슈 #85의 Pod 간 정상 flow 5-tuple RX/TX 추적이 dev cluster에서 정상 emit되는지 회귀 가드한다. dev cluster 전용이며 prod에서는 실행하지 않는다.

## 사전 조건

- dev cluster의 `netobs-agent` DaemonSet이 #85 series의 image로 업데이트되어 있음
- `netobs-agent`에 `NETOBS_FLOW_ALLOW_NAMESPACES=observability-test` env가 활성화되어 있음
- `kube-prometheus-stack-prometheus` Service가 monitoring namespace에 ready
- `observability-test` namespace의 `client` Pod와 `server` Pod가 자연 트래픽을 주고 받는 중

## 시나리오 개요

`observability-test` namespace의 client → server 자연 트래픽을 활용한다. 별도 stress workload를 띄우지 않으므로 외부 입력 없이 회귀 가드만 수행한다.

- 1차 가드: `count(netobs_flow_bytes_total{src_namespace="observability-test"}) >= 2` (egress + ingress 최소 2 entry)
- 2차 가드: `sum(netobs_flow_bytes_total{src_namespace="observability-test",src_workload="client"}) > 0` (client workload의 누적 bytes 양수. Deployment hash가 변동하는 `src_pod` 대신 안정적인 `src_workload` 라벨을 사용)

## 실행

```sh
./verify.sh
```

본 스크립트는 trigger 적용 없이 최대 300초 (15초 간격 polling) 안에 두 가드를 동시 통과하는지 확인한다.

## 종료 코드

- 0: 검증 통과
- 1: 검증 실패 (timeout, prometheus 미접근, opt-in 비활성 등)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `FLOW_NAMESPACE` | `ebpf-project` | netobs-agent DaemonSet의 namespace |
| `FLOW_ALLOW_NAMESPACE` | `observability-test` | 회귀 가드의 src_namespace 라벨 값 |
| `FLOW_TIMEOUT` | `300` | 통과 대기 timeout 초 |
| `FLOW_POLL_INTERVAL` | `15` | prometheus polling 주기 초 |
| `FLOW_PROM_NAMESPACE` | `monitoring` | prometheus Service namespace |
| `FLOW_PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus Service 이름 |
| `FLOW_PROM_PORT` | `9090` | prometheus Service port |

## 실패 시 진단

`[fail] timed out` 으로 떨어지면 다음 순서로 점검한다.

- `netobs-agent`의 env에 `NETOBS_FLOW_ALLOW_NAMESPACES=observability-test` 가 포함되어 있는지
- `observability-test` namespace의 `client` Pod가 `Running` 상태인지
- `client` → `server` 사이의 TCP 트래픽이 실제로 발생하는지 (kubectl exec로 connection 상태 확인)
- `netobs_bpf_map_utilization_ratio{map="flow_bytes"}` 가 0보다 큰지 (BPF map에 entry가 누적되는지 확인)
