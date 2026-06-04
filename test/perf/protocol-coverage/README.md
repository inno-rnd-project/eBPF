# protocol-coverage e2e 검증 시나리오

이슈 #103의 IPv6 와 UDP 추적 확장이 dev cluster에서 정상 attach + 기존 IPv4 회귀 차단 되는지 회귀 가드한다. dev cluster 전용이며 prod에서는 실행하지 않는다.

## 사전 조건

- netobs-agent image (`netobs-agent:<VERSION>`) 가 dev cluster에 push/load 되어 있는 상태
- `make deploy-netobs-dev` 적용으로 daemonset이 `Running` 상태
- dev cluster의 자연 IPv4 TCP 트래픽 활성 (Prometheus scrape, kube-apiserver 등)
- (선택) IPv6 트래픽 합성 부하 활성 시 ip_version=6 시리즈 자연 emit

## 시나리오 개요

- 1차 가드: 신규 6 kprobe (`tcp_v6_rcv`, `tcp_v6_do_rcv`, `udp_sendmsg`, `udp_recvmsg`, `udpv6_sendmsg`, `udpv6_recvmsg`) 의 `netobs_bpf_program_loaded == 1` 확인 (timeout 5분)
- 2차 가드: 기존 IPv4 TCP 회귀 차단. `count(netobs_pod_bytes_total) >= 1` 확인 (BPF struct 확장 후 누적 흐름 유지)
- 3차 가드 (warn only): `netobs_flow_bytes_total` 시리즈 존재. flow_bytes는 `NETOBS_FLOW_ALLOW_NAMESPACES` 환경 변수가 설정된 namespace만 emit이라 dev overlay 기본 동작에서는 비어있을 가능성

## 실행

```sh
./verify.sh
```

## 종료 코드

- 0: 1-2차 가드 통과 (3차 가드는 warn 처리, 종료 코드 영향 없음)
- 1: 6 kprobe 중 하나라도 attach 실패, 또는 기존 IPv4 TCP 회귀 (pod_bytes 시리즈 0)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `NETOBS_NAMESPACE` | `ebpf-project` | netobs-agent 가 위치한 namespace |
| `PROM_NAMESPACE` | `monitoring` | prometheus service 위치 namespace |
| `PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus service 이름 |
| `PROM_PORT` | `9090` | prometheus service port |
| `PROTO_TIMEOUT` | `300` | attach 확인 polling timeout (초) |
| `PROTO_POLL_INTERVAL` | `15` | polling 간격 (초) |

## 실패 시 진단

- `[wait] <symbol> attach 미확인 (loaded=0 또는 scrape 미완)` 가 반복되면 다음 점검
  - kernel 심볼 존재 확인: `kubectl debug node/<name> --image=alpine:3.18 -ti -- chroot /host grep -E ' <symbol>$' /proc/kallsyms`
  - netobs-agent log 의 `attached kprobe/<symbol>` 메시지 확인 (`kubectl logs -n ebpf-project -l app.kubernetes.io/name=netobs-agent`)
  - kernel 4.x 환경은 본 PR 비목표. 5.0+ 으로 upgrade 필요
- `[fail] IPv4 TCP 회귀: netobs_pod_bytes_total 시리즈 0` 으로 떨어지면 BPF struct 확장 후 누적 흐름이 깨진 상태. `go test -run TestFlowKeySize ./internal/netobs/ebpf/` 로 size 회귀 가드 통과 여부 확인 후 image 재배포
- `[warn] netobs_flow_bytes_total 시리즈 0` 은 dev overlay 의 기본 동작 (FlowGuard disabled). 운영자가 IPv6/UDP traffic 분포 시각화 가 필요 하면 `NETOBS_FLOW_ALLOW_NAMESPACES` 환경 변수를 dev overlay 에 추가

## 합성 IPv6/UDP 트래픽 정책

본 가드는 추가 합성 트래픽 Pod를 spawn하지 않는다. dev cluster의 자연 IPv4 트래픽만으로 회귀 차단을 확인한다. IPv6/UDP 트래픽 시각화 검증이 필요한 경우 별도 시나리오로 `socat` 또는 `iperf3 -V` listener/client Pod를 적용하되 검증 직후 즉시 `kubectl delete`로 정리 (`test/perf/*` 정리 정책 동일 적용).
