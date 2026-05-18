# correlation-stress

correlation-exporter 가 emit 하는 `correlation_noisy_neighbor_score` 의 Top-N 순위가 의도대로 산출되는지를 확인하기 위한 합성 부하 harness 다. 본 디렉토리의 매니페스트는 cluster 에 영구 배포되지 않으며 운영자가 검증 후 삭제한다.

## 시나리오

같은 노드에 3 개 Pod (`victim`, `suspect-sync`, `suspect-async`) 을 배치하고 `client` 는 nodeAffinity 의 DoesNotExist 로 `correlation-stress` 라벨이 없는 다른 노드에 배치한다. multi-node cluster 가 전제이며 single-node cluster 에서는 client Pod 가 Pending 으로 남는다.

- `victim`: nginx HTTP 서버. netobs 가 본 Pod 으로 향하는 트래픽의 `netobs_pod_stage_latency` 를 측정한다.
- `client`: wall-clock 10 분 cycle 의 0-5 분에 victim 에 HTTP GET (RPS 10) 부하, 5-10 분 idle. 다른 노드에 배치되므로 EnumeratePairs 의 페어 후보에 포함되지 않으며 단순 부하 발생기 역할만 한다.
- `suspect-sync`: 동일한 wall-clock 10 분 cycle 의 0-5 분에 CPU stress, 5-10 분 idle. client 와 정확히 같은 phase 라 victim latency 와 강한 cpu dimension 상관을 보여야 한다.
- `suspect-async`: wall-clock 7 분 cycle 의 0-3 분에 CPU stress. client 의 10 분 cycle 과 lcm 이 70 분이라 phase drift 가 영구적으로 발생해 sync 보다 약한 상관이 잡혀야 한다.

세 Pod 의 burst 주기는 모두 5 분 이상으로 exporter step 30s 의 10 배 이상이다. burst 주기가 step 보다 짧으면 step aggregation 으로 burst 의 동기성이 평균화되어 사라지므로 step × 10 이 안전한 하한이다. 또한 모든 burst 가 `date +%s` modulo 패턴으로 wall-clock 정각에 동기화되어 Pod 별 시작 시점 차이로 인한 phase drift 가 없다.

기대 결과는 30 분 이상 운영 후 (`WINDOW=30m` 충족 + 최소 3 burst cycle 관측) 다음과 같다.

- `correlation_noisy_neighbor_score{victim_pod="victim",suspect_pod="suspect-sync",resource_dimension="cpu",rank="1"}` 가 0.7 이상
- 동일 victim 의 `suspect_pod="suspect-async"` series 는 rank 1 이 아니거나 score 가 sync 의 70 % 이하

## 사용법

1. 노드 선택자 라벨 부여 (한 노드 고정).

```sh
kubectl label node <node-name> correlation-stress=victim --overwrite
```

2. harness 적용.

```sh
kubectl apply -k test/correlation-stress/
```

3. 1 시간 후 결과 확인 (port-forward 사용).

```sh
kubectl port-forward -n monitoring svc/kube-prometheus-stack-prometheus 9090:9090 &
curl -s 'http://localhost:9090/api/v1/query?query=correlation_noisy_neighbor_score{victim_pod="victim"}' | jq
```

correlation-debug CLI 와의 cross-check 는 다음과 같다.

```sh
./bin/correlation-debug -prometheus-url http://localhost:9090 -window 1h | \
  jq '[.[] | select(.pair.dst_pod == "victim")] | sort_by(-.max_abs_value) | .[:5]'
```

4. 검증 종료 후 정리.

```sh
kubectl delete -k test/correlation-stress/
kubectl label node <node-name> correlation-stress-
```

## 비목표

본 harness 는 correlation-exporter 의 정확성 검증용이다. netobs / gpuobs 자체 동작이나 PrometheusRule 의 recording rule 산출은 본 harness 의 범위 밖이며 별도 시리즈의 합성 시나리오에서 다룬다.
