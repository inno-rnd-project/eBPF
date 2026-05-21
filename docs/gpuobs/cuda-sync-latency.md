# CUDA stream / event 동기화 latency 운영 가이드

`gpuobs_cuda_stream_synchronize_seconds`, `gpuobs_cuda_event_synchronize_seconds`, `gpuobs_cuda_stream_wait_event_total` 3 종과 `GPUObsCudaStreamWaitHigh` alert 로 CUDA 워크로드의 stream 단위 host wait time 을 진단하는 운영 워크플로다. 본 도구는 #67 에서 도입되었으며 기존 kernel launch / memcpy 카운터를 보완해 stream 동기화로 인한 GPU 워크로드 병목을 가시화한다.

## 메트릭과 alert 카탈로그

- `gpuobs_cuda_stream_synchronize_seconds_*{node, src_namespace, src_pod, src_pod_uid, gpu_uuid}` histogram. `cuStreamSynchronize` 의 entry-exit 페어 host wait time 분포. bucket 은 `prometheus.ExponentialBuckets(1e-6, 2, 20)` 으로 1us 시작 2배 곱 20 bucket
- `gpuobs_cuda_event_synchronize_seconds_*` histogram. `cuEventSynchronize` 의 entry-exit 페어 host wait time. 동일 bucket
- `gpuobs_cuda_stream_wait_event_total{...}` counter. `cuStreamWaitEvent` 호출 누적. cuStreamWaitEvent 는 host blocking 없이 stream 에 wait 명령을 enqueue 만 하고 즉시 반환하는 non-blocking call 이라 latency histogram 의 진단 가치가 없어 호출 빈도 counter 로만 노출
- `gpuobs_cuda_symbol_available{symbol="cuStreamSynchronize"}` 등 3 종. attach 성공 시 1, 실패 시 0
- `GPUObsCudaStreamWaitHigh` alert. `cuStreamSynchronize` p99 가 100ms 를 5 분 지속 초과 시 발화

## stage / counter 분리 의도

receive path 의 4 stage 분류와 동일하게 동기화 3 종도 호출 의미가 다르다.

- `cuStreamSynchronize` host 가 stream 의 모든 선행 작업 완료를 기다리는 blocking call. wait time = GPU 작업 잔량 + 큐 대기 시간. histogram 으로 분포 노출
- `cuEventSynchronize` host 가 event 가 recorded 되는 시점을 기다리는 blocking call. wait time = event 직전 작업 완료 시간. histogram 으로 분포 노출
- `cuStreamWaitEvent` GPU 측 dependency 만 추가하는 non-blocking call. host wall time 은 거의 0. counter 로 호출 빈도만 노출

## 진단 워크플로

1. alert 발화 시 payload 의 `src_namespace`, `src_pod`, `node` 로 영향 Pod 식별
2. 동일 Pod 의 `gpuobs_cuda_kernel_launches_total` rate 와 `gpuobs_cuda_h2d_bytes_total` / `gpuobs_cuda_d2h_bytes_total` rate 비교

   ```sh
   PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
   kubectl exec -n monitoring $PROM_POD -c prometheus -- \
     wget -qO- 'http://localhost:9090/api/v1/query?query=rate(gpuobs_cuda_stream_synchronize_seconds_sum{src_pod="POD"}[5m])' | jq
   ```

3. wait time 의 dominant 가 kernel 실행 vs memcpy 인지 추적
   - kernel launch rate 높음 + sync wait 높음. kernel 자체 GPU 점유 시간이 길다는 신호. 워크로드의 kernel batch 크기 조정 검토
   - memcpy rate 높음 + sync wait 높음. PCIe 또는 host I/O 가 GPU 작업을 따라가지 못함. `node:gpu_pcie_saturation_score:5m` 함께 점검
4. `gpuobs_cuda_stream_wait_event_total` rate 가 함께 상승하면 inter-stream dependency 가 많은 워크로드 (pipeline parallelism 등) 라 stream 재설계 가능성 검토
5. dominant cause weighting (#66) 의 `pod:host_compute_stall_score:5m` 와 cross-reference. GPU idle 이 동반되면 host 측 CUDA launch rate 부족 또는 GPU memory 포화가 추가 원인일 수 있다

## 검증 시나리오

dev cluster 에서 cuStreamSynchronize, cuEventSynchronize, cuStreamWaitEvent 3 종 hook 이 sample 을 수집하는지 회귀 가드한다. ResNet50 bench 는 launch rate 최대화를 위해 의도적으로 synchronize 호출을 회피해 본 검증에 부적합하므로 명시적 stream / event 를 사용하는 별도 워크로드 `test/perf/pytorch-cuda-stream-sync-bench.yaml` 을 적용한다.

```sh
kubectl apply -f test/perf/pytorch-cuda-stream-sync-bench.yaml
kubectl wait --for=condition=ready --timeout=10m -n ebpf-project pod/pytorch-cuda-stream-sync-bench
```

2 분 후 다음 쿼리들이 모두 양수를 반환하면 회귀 통과로 본다.

```sh
PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
for q in \
  'sum(rate(gpuobs_cuda_stream_synchronize_seconds_count[2m]))' \
  'sum(rate(gpuobs_cuda_event_synchronize_seconds_count[2m]))' \
  'sum(rate(gpuobs_cuda_stream_wait_event_total[2m]))'; do
  kubectl exec -n monitoring $PROM_POD -c prometheus -- \
    wget -qO- "http://localhost:9090/api/v1/query?query=${q}" | jq
done
```

`gpuobs_cuda_symbol_available{symbol="cuStreamSynchronize"} == 1` 도 함께 확인해 attach 성공 여부를 가드한다.

회귀 가드 통과 직후 다음 명령으로 bench Pod 를 정리한다. GPU 부하 워크로드를 dev 클러스터에 상주시키면 GPU idle dominant cause 검증 같은 다른 회귀에 영향을 주고 전력 / 열 부담도 누적된다.

```sh
kubectl delete -f test/perf/pytorch-cuda-stream-sync-bench.yaml
```

## 카디널리티 분석

scrape 시점 시리즈 수 상한.

- 2 histogram x (20 bucket + count + sum) = Pod 당 44 시리즈
- 1 counter = Pod 당 1 시리즈
- 총 Pod 당 45 시리즈

활성 GPU Pod 50 기준 약 2250 시리즈로 cluster 단위 cap 에 들어간다. stream / event handle 은 cardinality 폭발 방지로 메트릭 라벨에 포함되지 않으며, `gpu_uuid` 는 `cuda_tid_device` map 의 식별 결과를 따르고 미식별 시 `unknown` fallback 을 사용한다.

## 알려진 한계

- NCCL collective 통신 (`ncclAllReduce` 등) symbol 의 latency 는 본 PR 범위 밖이다. dev cluster 의 single GPU 환경에서 trigger 가 불가능해 multi-GPU 환경 follow-up 으로 분리한다
- CUDA Runtime API 의 동일 symbol (`cudaStreamSynchronize` 등) 의 latency 측정은 본 PR 범위 밖이다. Driver API hook 으로 PyTorch 같은 Runtime API 워크로드도 대부분 잡히지만 Runtime API 측 직접 hook 은 follow-up 이슈로 분리한다
- `cuStreamWaitEvent` 는 non-blocking call 이라 host wall time histogram 의 진단 가치가 없어 counter 로만 노출. 실제 GPU 측 wait 시간은 stream / event handle 단위 추적이 필요하나 cardinality 제약으로 본 PR scope 밖
- entry-exit 페어 측정은 process 가 sync 호출 중에 종료되거나 longjmp 로 비정상 return 되면 uretprobe 가 miss 되어 해당 sample 이 누락된다. `sync_starts` map 은 LRU_HASH 라 stale entry 가 자동 evict 되며 metric 동작에는 영향이 없다
