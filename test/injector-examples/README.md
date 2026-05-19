# workload-injector 예제

`cmd/workload-injector` 의 cpu / network / gpu 3 종 부하를 dev 클러스터에서 즉시 실행 가능한 Job 매니페스트다. 운영자가 본 예제를 단일 명령으로 적용해 메트릭 emit 흐름을 검증한다.

## 사전 조건

- `make deploy-injector-dev` 가 적용되어 ServiceAccount / ClusterRole / ConfigMap / PrometheusRule / PodMonitor 가 cluster 에 존재함
- dev cluster Node 가 `environment=dev` 라벨을 보유함 (안전 가드 통과 조건)
- target Pod 가 `correlation-stress` namespace 의 `victim` 으로 존재 (`kubectl apply -k test/correlation-stress/`)
- gpu 예제만 추가로 NVIDIA GPU Operator 가 설치된 노드와 GPU 점유 가능한 target Pod 가 필요

## 사용법

### cpu kind (5 분 부하)

```sh
kubectl apply -f test/injector-examples/cpu.yaml

# 메트릭 확인
PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=injector_active' | jq

# 5 분 후 결과 확인
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=correlation_blast_radius_score{kind=%22cpu%22}' | jq

# Job 정리 (ttlSecondsAfterFinished 로 자동 삭제되지만 수동 정리 가능)
kubectl delete -f test/injector-examples/cpu.yaml
```

### network kind

```sh
kubectl apply -f test/injector-examples/network.yaml
```

network kind 는 iperf3 server (target 노드) 와 client (별도 노드) 두 Pod 를 spawn 한다. server 측 트래픽이 victim Pod 의 latency 에 영향을 주는지 측정한다.

### gpu kind

```sh
kubectl apply -f test/injector-examples/gpu.yaml
```

gpu kind 는 NVIDIA GPU Operator 가 설치되지 않은 노드에서는 Pod 가 Pending 으로 남는다. dev cluster 의 gpu 노드에서만 사용한다.

## 검증 절차

본 이슈 #52 의 수용 조건 4 가지를 다음 절차로 검증한다.

1. `kubectl apply -f cpu.yaml` 직후 `injector_active=1` 시계열이 PodMonitor scrape (15s) 내에 emit 되는지 확인
2. `DURATION` (기본 5 분) 종료 시점에 `injector_active` 가 `=0` 으로 transition 되고 linger 30 초 동안 유지되다 사라지는지 확인
3. `correlation_blast_radius_score` 가 victim Pod 별로 emit 되며 라벨 셋이 `target_namespace, target_pod, target_node, victim_namespace, victim_pod, victim_pod_uid, kind` 7 개를 모두 포함하는지 확인
4. `correlation_noisy_neighbor_score` (#51 exporter) 와 `injector_active=1` 윈도우가 시간상 align 되는지 Grafana cross-reference panel 로 확인

## 트러블슈팅

- Job 의 Pod 가 ImagePullBackOff → `make image-build-workload-injector` 가 dev cluster 노드의 docker daemon 에서 실행되었는지 확인
- safety gate 거부 → Node 라벨 `environment=dev` 가 있는 노드가 1 개 이상인지 확인
- spawn 된 stress Pod 가 종료되지 않음 → `InjectorStuck` alert 발화 대기 또는 `kubectl delete pod -l app.kubernetes.io/component=stress` 로 수동 cleanup
