# bpf-map-race e2e 회귀 가드 (#107)

이슈 #107 의 BPF map race condition audit 의 e2e 회귀 가드 스크립트다. dev cluster 의 multi-pod / multi-CPU 환경에서 의도적 race 자극 시나리오로 BPF map 의 일관성 정합을 검증한다. Go race detector 가 잡지 못하는 BPF ↔ userspace 경계 race 의 운영적 식별 가드 역할.

## 실행

```sh
test/perf/bpf-map-race/verify.sh
```

스크립트가 자동으로 `iperf3-multistream.yaml` 워크로드 (cross-node iperf3 `-P 16` parallel streams) 를 적용 후 검증 직후 trap 으로 정리한다. env 로 override 가능.

- `TRAFFIC_DURATION` (기본 90s): 1차 가드 의 counter monotonic 모니터링 시간
- `POLL_INTERVAL` (기본 10s): counter 샘플링 간격
- `NETOBS_NAMESPACE` (기본 `ebpf-project`): 워크로드 배포 namespace
- `PROM_NAMESPACE` (기본 `monitoring`): Prometheus Service namespace
- `PROM_SVC` (기본 `kube-prometheus-stack-prometheus`): Prometheus Service 이름

## 가드 단계

- **1차 (fail-on-miss)**: cross-node iperf3 `-P 16` 트래픽 발생 중 `sum(netobs_flow_bytes_total)` 와 `sum(netobs_pod_bytes_total)` 의 counter monotonic 증가 검증. race 발생 시 일시적 감소 또는 stale read 의 signature 가 본 가드에 잡힘
- **2차 (warn-only)**: 트래픽 발생 중 `netobs_bpf_map_utilization_ratio{map="starts"}` 의 정상 범위 (0.0-1.0) 정합. starts LRU evict timing 의존 race 의 signature
- **3차 (warn-only)**: `netobs_bpf_ringbuf_drops_total` rate 가 정상 범위 (<100/s). ringbuf reserve/submit pair atomicity 결함의 간접 signature

## 한계

- Go race detector 와 달리 본 가드는 비결정적 timing 의존이라 false negative (race 발생 하지만 검증 시점에 시그널 없음) 가 가능. 따라서 1차만 fail-on-miss, 2-3차는 warn-only.
- `flow_bytes` 는 dev overlay 의 `NETOBS_FLOW_ALLOW_NAMESPACES` env 미설정으로 자연 0 일 수 있음. counter 증가 미관측은 warn 처리.
- iperf3 컨테이너 가 dev cluster 의 image pull policy `IfNotPresent` 로 자연 캐시. air-gapped 환경에서는 사전에 image 가 노드에 푸시되어 있어야 함.
- cross-node 흐름이 의도라 `ebpf-worker1` 과 `ebpf-worker2` 두 노드가 모두 Ready 상태여야 통과 가능.

## 실행 환경 제약

본 스크립트는 Prometheus 의 `ClusterIP` 를 직접 조회 해 curl 로 query 를 보낸다. `ClusterIP` 는 쿠버네티스 클러스터 내부 네트워크에서만 라우팅 가능 하므로 본 스크립트의 실행 환경은 다음 중 하나여야 한다.

- dev cluster 의 control-plane 또는 worker 노드에서 직접 실행
- 클러스터 내부 Pod 에서 실행
- 클러스터 외부에서 실행할 경우 사전에 `kubectl port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9090:9090` 으로 port-forward 설정 후 `PROM_NAMESPACE` 와 `PROM_SVC` 를 localhost 대체 endpoint 로 override

본 PR 의 dev cluster 검증은 첫 번째 패턴 (노드에서 직접 실행) 으로 수행 되었다. Prometheus 가 도달 불가능한 환경 에서는 스크립트 시작 단계의 `/-/ready` 헬스 체크가 fail-fast 로 종료 시켜 false positive (Prometheus 장애 시 모든 메트릭이 0 으로 반환 되어 monotonic 검증이 항상 pass 하는 결함) 를 차단 한다.
