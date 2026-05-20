# GPU 유휴 dominant cause 가중치 ranking 운영 가이드

`gpu_idle_cause_weight:5m`, `cluster:gpu_idle_dominant_cause:5m`, `GPUIdleDominantCauseSwitch` 3 종으로 GPU 유휴 상태의 dominant cause 를 정량 식별하는 운영 워크플로다. 본 도구는 #66 에서 도입되었으며 `GPUIdleWith*` 5 종 alert 가 동시 firing 할 때 운영자가 manual 판단하던 dominant cause 식별을 rule 기반으로 자동화한다.

## 메트릭과 alert 카탈로그

- `cluster:gpu_pcie_saturation_score:5m`, `cluster:pod_cpu_throttle_score:5m`, `cluster:pod_memory_pressure_score:5m`, `cluster:pod_network_pressure_score:5m`, `cluster:pod_host_compute_stall_score:5m`. 5 cause base score 의 cluster max rollup
- `gpu_idle_cause_sum:5m`. 5 cause base score 의 합 (정규화 분모)
- `gpu_idle_cause_weight:5m{cause}`. 5 cause 정규화 가중치. cluster 의 어느 노드라도 `node:gpu_idle:5m > 0.5` 일 때만 emit
- `cluster:gpu_idle_dominant_cause:5m{cause}`. weight 최대값을 가진 cause 1 종을 단일 시리즈로 노출. 동률 시 cause enum 사전순 가장 앞 라벨이 채택
- `gpu_idle_dominant_cause_indicator:5m{cause}`. cause 별 0/1 indicator. `GPUIdleDominantCauseSwitch` alert 가 changes() 합산에 사용
- `GPUIdleDominantCauseSwitch` alert. 10 분 안에 2 회 이상 swap 시 발화 (changes 합산 임계 4)

## cause 라벨 enum

`cause` 라벨 값은 다음 5 종으로 고정한다. `GPUIdleWith*` 5 종 alert 의 cause 매핑과 1:1 일치한다.

- `pcie_saturation` (`GPUIdleWithPCIeSaturation`)
- `network_pressure` (`GPUIdleWithNetworkPressure`)
- `cpu_throttle` (`GPUIdleWithCPUThrottle`)
- `memory_pressure` (`GPUIdleWithMemoryPressure`)
- `host_compute_stall` (`GPUIdleWithHostComputeStall`)

## 진단 워크플로

1. dashboard `observability overview` 의 cluster overview row 에서 `GPU idle dominant cause` stat 패널 확인. `N/A` 면 GPU 가 유휴 상태가 아니며 cause weight 가 산출되지 않는 시간대다
2. dominant cause 라벨이 식별되면 해당 cause 의 base score 와 cluster 단위 rollup 값을 비교해 worst-case 노드 또는 Pod 를 식별

   ```sh
   PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
   kubectl exec -n monitoring $PROM_POD -c prometheus -- \
     wget -qO- 'http://localhost:9090/api/v1/query?query=cluster:gpu_idle_dominant_cause:5m' | jq
   kubectl exec -n monitoring $PROM_POD -c prometheus -- \
     wget -qO- 'http://localhost:9090/api/v1/query?query=gpu_idle_cause_weight:5m' | jq
   ```

3. 5 cause weight 의 합이 1.0 인지 확인해 정규화가 정상 동작하는지 점검

   ```sh
   kubectl exec -n monitoring $PROM_POD -c prometheus -- \
     wget -qO- 'http://localhost:9090/api/v1/query?query=sum(gpu_idle_cause_weight:5m)' | jq
   ```

4. dominant cause 가 식별된 case 별 drill-down 경로

   - `cpu_throttle`. `docs/correlation/diagnosis-gpu-idle.md` 의 CPU throttle 절과 `pod:cpu_throttle_score:5m{node, src_pod}` 시계열
   - `memory_pressure`. `pod:memory_pressure_score:5m` 와 `kube_pod_container_resource_limits{resource="memory"}` 비교
   - `network_pressure`. `pod:network_throughput_score:5m` 와 `pod:network_retrans_score:5m` 양쪽 확인
   - `pcie_saturation`. `node:gpu_pcie_saturation_score:5m` 와 PCIe link generation/width 점검
   - `host_compute_stall`. `gpuobs_cuda_kernel_launches_total` rate 와 `pod:gpu_memory_utilization_ratio:5m` 점검

5. `GPUIdleDominantCauseSwitch` 발화 시 cause 식별 자체가 불안정한 워크로드를 의심한다. `gpu_idle_dominant_cause_indicator:5m` 시계열에서 어느 두 cause 가 cycling 하는지 확인 후 두 cause 의 base score 추세를 비교

## 검증 시나리오

dev cluster 에서 `workload-injector` 의 cpu kind 합성 부하로 dominant cause 가 `cpu_throttle` 로 식별되는지 회귀 가드한다.

```sh
kubectl exec -n correlation-stress workload-injector -- /injector \
  --kind cpu --duration 600 --intensity 0.9
```

10 분 후 다음 쿼리가 `cause="cpu_throttle"` 한 줄을 반환하면 회귀 통과로 본다.

```sh
PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=cluster:gpu_idle_dominant_cause:5m' | jq
```

## 카디널리티 분석

scrape 시점 시리즈 수 상한.

- `cluster:gpu_*_score:5m` 5 종. 각 1 시리즈 (cluster scalar). 총 5 시리즈
- `gpu_idle_cause_sum:5m`. 1 시리즈
- `gpu_idle_cause_weight:5m`. cause 라벨 5 종 x 1 시리즈 = 5 시리즈
- `cluster:gpu_idle_dominant_cause:5m`. 1 시리즈 (topk(1))
- `gpu_idle_dominant_cause_indicator:5m`. cause 라벨 5 종 x 1 시리즈 = 5 시리즈

총 17 시리즈 상한. GPU 유휴 상태가 아닌 시간대는 idle 게이팅으로 일부 시리즈가 emit 되지 않아 실 운영에서는 더 적다.

## 알려진 한계

- single GPU cluster 전제. multi-GPU 노드의 GPU 별 cause 분리는 본 PR 범위 밖이며 `node:gpu_idle:5m` 의 노드 단위 평균에 흡수된다
- `cluster:pod_network_pressure_score:5m` 는 throughput saturation score 만 사용한다. retrans 신호는 `GPUIdleWithNetworkPressure` alert 의 OR 보조 신호로 남고 cause weighting 합산에 합치면 threshold 스케일 차이로 cause 간 비교 의미가 흐려진다
- RTX 3090 Ti 같은 consumer GPU 는 ECC 미지원, throttle reason 일부 미발생 등으로 일부 cause 의 base score 가 항상 0 인 경우가 많다. 본 환경에서는 0 cause 가 weighting 에서 자연 제외된다
- LLM 기반 자연어 cause 설명은 본 PR 범위 밖이다. rule-based weighting 까지가 책임이다
