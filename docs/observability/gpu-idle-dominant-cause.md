# GPU 유휴 dominant cause 가중치 ranking 운영 가이드

`gpu_idle_cause_weight:5m`, `cluster:gpu_idle_dominant_cause:5m`, `GPUIdleDominantCauseSwitch` 3 종으로 GPU 유휴 상태의 dominant cause 를 정량 식별하는 운영 워크플로다. 본 도구는 #66 에서 도입되었으며 `GPUIdleWith*` 5 종 alert 가 동시 firing 할 때 운영자가 manual 판단하던 dominant cause 식별을 rule 기반으로 자동화한다. #101 에서 victim Pod 단위 cause attribution, secondary cause top3 비교, ambiguous dominant 감지 alert 가 추가되어 cluster 단위 dominant 단일 시리즈 외에 victim 별 dominant cause 와 secondary cause 까지 한 가이드 안에서 진단 가능하다.

## 메트릭과 alert 카탈로그

cluster 차원 (#66 도입).

- `pod:*_score_rise:5m` (#244). pod 계열 cause score 의 baseline (30m~1h30m 전 평균) 대비 상승분. limit 에 상시 근접한 관측 인프라 pod 처럼 GPU 유휴와 무관하게 일정한 신호는 0 에 수렴하고, 유휴와 함께 나타난 신규 압박만 남는다
- `cluster:gpu_idle_*_rise:5m` (#244). pod 계열 cause 5종 (cpu_throttle, memory_pressure, network_pressure, cgroup_contention, host_compute_stall) rise 의 GPU 유휴 노드 스코프 max rollup. weight 와 sum 이 소비하는 base 이며, `cluster:pod_*_score:5m` (클러스터 전체 pod max) 는 운영자용 절대값 worst-case 뷰로 유지된다
- `cluster:gpu_pcie_saturation_score:5m` 등 device 계열 base score (pcie/dcgm/nccl/thermal). GPU 스코프 신호라 rise 없이 절대값 그대로 weight 에 편입
- `gpu_idle_cause_sum:5m`. cause base (pod 계열 rise 5종 + device 계열 4종) 의 합 (정규화 분모)
- `gpu_idle_cause_weight:5m{cause}`. 5 cause 정규화 가중치. cluster 의 어느 노드라도 `node:gpu_idle:5m > 0.5` 일 때만 emit
- `cluster:gpu_idle_dominant_cause:5m{cause}`. weight 최대값을 가진 cause 1 종을 단일 시리즈로 노출. 동률 시 cause enum 사전순 가장 앞 라벨이 채택
- `gpu_idle_dominant_cause_indicator:5m{cause}`. cause 별 0/1 indicator. `GPUIdleDominantCauseSwitch` alert 가 changes() 합산에 사용
- `gpu_idle_cause_weight_top3:5m{cause}`. cluster 단위 top3 cause 시리즈. secondary 와 tertiary cause 비교용 dashboard panel 입력
- `GPUIdleDominantCauseSwitch` alert. 10 분 안에 2 회 이상 swap 시 발화 (changes 합산 임계 4)
- `GPUIdleDominantCauseAmbiguous` alert. top1 과 top2 weight 격차 < 0.1 이면서 magnitude (top1) > 0.3 일 때 5 분 지속 시 발화

node 차원 (#256 도입). cluster 와 pod 사이의 노드 scope 계층으로, 노드 하나의 GPU 가 왜 노는지를 원인별로 설명한다. cause 정의와 정규화 규약은 cluster rule 과 동일하다.

- `node:gpu_idle_cause_score:5m{node,cause}`. 노드별 9 cause base score. pod 계열 5 종은 `pod:*_score_rise:5m` 를 node 별 max 로 집계하고, device 계열 4 종은 `node:gpu_*_score:5m` 노드 신호에 유휴 게이팅만 덧댄다. `node:gpu_idle:5m > 0.5` 게이팅으로 유휴 노드에서만 산출되고 node 신호가 없는 원인은 series 가 자연 제외된다
- `node:gpu_idle_cause_sum:5m{node}`. 노드별 cause base 합 (정규화 분모)
- `node:gpu_idle_cause_weight:5m{node,cause}`. 노드별 정규화 가중치. cluster weight 와 동일하게 분모 노이즈 하한 (> 0.05) 을 둔다
- `node:gpu_idle_dominant_cause:5m{node,cause}`. 노드별 dominant cause 단일 시리즈 (topk by node + cluster 와 동일 tie-breaker)

victim 차원 (#101 도입).

- `pod:gpu_idle_cause_score:5m{victim_namespace,victim_pod,cause}`. 5 cause base score 의 victim 단위 정합 helper. PCIe 는 `kube_pod_info` 매핑으로 GPU 노드 Pod 에 broadcast, 4 cause 는 기존 pod 단위 score 를 label_replace 로 victim 라벨 alias
- `pod:gpu_idle_cause_sum:5m{victim_namespace,victim_pod}`. victim 별 5 cause base score 합 (정규화 분모)
- `pod:gpu_idle_cause_weight:5m{victim_namespace,victim_pod,cause}`. victim 별 정규화 가중치. cluster 차원 weight 와 동일 idle 게이팅
- `victim:gpu_idle_dominant_cause:5m{victim_namespace,victim_pod,cause}`. victim 별 dominant cause 단일 시리즈 (topk by victim + tie-breaker)
- `victim:gpu_idle_dominant_cause_indicator:5m{victim_namespace,victim_pod,cause}`. victim 별 cause 0/1 indicator
- `pod:gpu_idle_cause_weight_top3:5m{victim_namespace,victim_pod,cause}`. victim 별 top3 cause 시리즈
- `VictimGPUIdleDominantCauseAmbiguous` alert. victim 별 ambiguous 감지. cluster alert 와 동일 격차 / magnitude 게이팅 을 victim 차원으로 적용

## cause 라벨 enum

`cause` 라벨 값은 다음 5 종으로 고정한다. `GPUIdleWith*` 5 종 alert 의 cause 매핑과 1:1 일치한다.

- `pcie_saturation` (`GPUIdleWithPCIeSaturation`)
- `network_pressure` (`GPUIdleWithNetworkPressure`)
- `cpu_throttle` (`GPUIdleWithCPUThrottle`)
- `memory_pressure` (`GPUIdleWithMemoryPressure`)
- `host_compute_stall` (`GPUIdleWithHostComputeStall`)

## dimension 4 종과 cause 5 종 매핑

`internal/correlation/dominant.go` 의 `ComputeDominantDimension` 은 noisy neighbor 차원을 4 dimension (`cpu` 와 `gpu` 와 `memory` 와 `network`) 으로 산정하고, `gpu_idle_cause_weight:5m` 의 cause 는 5 종이다. 두 체계는 다음 매핑 표 로 정합된다. dimension `gpu` victim 발견 시 cause weight 5 종 중 어느 두 cause 를 우선 보아야 하는지 본 표 로 결정한다.

| dimension | 매핑 cause | 설명 |
|---|---|---|
| `cpu` | `cpu_throttle` | cAdvisor CFS throttle 비율 base |
| `memory` | `memory_pressure` | 컨테이너 working_set 대비 limit 비율 base |
| `network` | `network_pressure` | Pod 단위 throughput saturation base (retrans 는 OR 보조 신호) |
| `gpu` | `pcie_saturation` 과 `host_compute_stall` | GPU 의 device 측 (PCIe link) 압박과 host 측 (CUDA launch / device memory) 압박 양쪽 흡수 |

dimension `gpu` victim 운영 시 weight 가장 큰 두 cause 가 보통 `pcie_saturation` 또는 `host_compute_stall` 이다. `gpu_idle_cause_weight_top3:5m` 또는 `pod:gpu_idle_cause_weight_top3:5m` 시계열로 secondary cause 와의 격차를 확인 후 어느 device 측 / host 측 압박이 우세한지 식별한다.

## 진단 워크플로

1. dashboard `observability overview` 의 cluster overview row 에서 `GPU idle dominant cause` stat 패널 확인. `N/A` 면 GPU 가 유휴 상태가 아니거나 5 cause 모두 0 인 시간대다 (idle 게이팅 실패 또는 cause 미식별 양쪽 모두 동일 메시지로 떨어진다)
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

## victim 단위 워크플로 (#101)

cluster 단위 단일 dominant cause 가 식별되더라도 multi-tenant 환경에서는 victim Pod 별로 dominant cause 가 다를 수 있다. dimension `gpu` victim 발견 시 다음 흐름으로 victim 단위 진단을 수행한다.

1. `correlation_noisy_neighbor_score{resource_dimension="gpu"}` 의 victim 라벨 (`victim_namespace`, `victim_pod`) 을 확인. 본 victim 이 GPU dimension 압박을 받는 후보다
2. `gpu-network-correlation` dashboard 의 `$src_namespace` 와 `$src_pod` variable 을 본 victim 으로 설정. panel 2 (cause weight stacked) 와 panel 3 (dominant cause indicator timeline) 이 victim 단위 cause 분포를 노출
3. victim 단위 dominant cause 직접 query 로 단일 cause string 식별

   ```sh
   kubectl exec -n monitoring $PROM_POD -c prometheus -- \
     wget -qO- 'http://localhost:9090/api/v1/query?query=victim:gpu_idle_dominant_cause:5m{victim_namespace="<ns>",victim_pod="<pod>"}' | jq
   ```

4. secondary cause 와의 격차 확인. `pod:gpu_idle_cause_weight_top3:5m{victim_namespace="<ns>",victim_pod="<pod>"}` 3 cause 시리즈 비교. top1 과 top2 격차가 작으면 (< 0.1) cause 식별이 동률 ambiguous 상태이므로 manual 진단 으로 전환
5. `VictimGPUIdleDominantCauseAmbiguous` alert 발화 시 victim 단위 cause weight 가 동률 ambiguous 임을 의미. 본 alert 는 cause 식별 자동 rule 의 신뢰도 저하 신호 이며 즉시 대응 의무는 없으나 운영자가 본 alert 발화 victim 에 대해서는 dashboard panel 의 stacked cause weight 를 직접 확인 한 뒤 dominant cause 라벨에 의존하지 말고 운영 판단 필요

## 검증 시나리오

dev cluster 에서 `workload-injector` 의 cpu kind 합성 부하로 dominant cause 가 `cpu_throttle` 로 식별되는지 회귀 가드한다. `workload-injector` 는 Job 매니페스트로 한 번 spawn 되는 binary 이며 `test/injector-examples/cpu.yaml` 의 샘플 Job 을 그대로 사용한다. 샘플의 image 태그는 historical 값으로 박혀 있어 본 검증 전에 현재 cluster 의 build / load 정책에 맞춰 (`workload-injector:$(cat VERSION)` 또는 registry path) 조정해야 한다.

```sh
sed -i "s|image: workload-injector:.*|image: workload-injector:$(cat VERSION)|" \
  test/injector-examples/cpu.yaml
kubectl apply -f test/injector-examples/cpu.yaml
kubectl wait --for=condition=complete --timeout=10m \
  -n ebpf-project job/workload-injector-cpu-example
```

샘플 Job 은 `correlation-stress/victim` Pod 에 `INTENSITY=500m`, `DURATION=5m` 의 CPU 부하를 5 분간 발사한다. 부하 종료 후 다음 쿼리가 `cause="cpu_throttle"` 한 줄을 반환하면 회귀 통과로 본다.

```sh
PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=cluster:gpu_idle_dominant_cause:5m' | jq
```

## 카디널리티 분석

scrape 시점 시리즈 수 상한.

cluster 차원 (#66).

- `cluster:gpu_*_score:5m` 5 종. 각 1 시리즈 (cluster scalar). 총 5 시리즈
- `gpu_idle_cause_sum:5m`. 1 시리즈
- `gpu_idle_cause_weight:5m`. cause 라벨 5 종 x 1 시리즈 = 5 시리즈
- `cluster:gpu_idle_dominant_cause:5m`. 1 시리즈 (topk(1))
- `gpu_idle_dominant_cause_indicator:5m`. cause 라벨 5 종 x 1 시리즈 = 5 시리즈
- `gpu_idle_cause_weight_top3:5m`. cause 라벨 3 종 x 1 시리즈 = 3 시리즈

victim 차원 (#101). `V` 는 cluster 의 활성 Pod 수.

- `pod:gpu_idle_cause_score:5m`. cause 5 종 x V (PCIe 는 GPU 노드 Pod 만)
- `pod:gpu_idle_cause_sum:5m`. V 시리즈
- `pod:gpu_idle_cause_weight:5m`. cause 5 종 x V (idle 게이팅으로 일부 skip)
- `victim:gpu_idle_dominant_cause:5m`. V 시리즈 (victim 별 dominant cause 1 종)
- `victim:gpu_idle_dominant_cause_indicator:5m`. cause 5 종 x V (cause indicator)
- `pod:gpu_idle_cause_weight_top3:5m`. cause 3 종 x V

cluster 차원 총 20 시리즈 상한. victim 차원 총 약 `20 * V` 시리즈 상한 (V = 활성 Pod 수, 5V + V + 5V + V + 5V + 3V). GPU 유휴 상태가 아닌 시간대는 idle 게이팅으로 일부 시리즈가 emit 되지 않아 실 운영에서는 더 적다. dev 클러스터의 V ≈ 50 기준 victim 차원 약 1000 시리즈 수준.

## 알려진 한계

- single GPU cluster 전제. multi-GPU 노드의 GPU 별 cause 분리는 본 PR 범위 밖이며 `node:gpu_idle:5m` 의 노드 단위 평균에 흡수된다
- network cause weighting 은 canonical `pod:network_pressure_score:5m` (throughput 과 retrans 의 element-wise max, #154) 의 rise 를 쓴다. `GPUIdleWithNetworkPressure` alert 는 throughput (임계 0.7) 과 retrans (임계 0.05) 의 스케일이 달라 서브 신호 rise 2종으로 분리 판정을 유지한다
- RTX 3090 같은 consumer GPU 는 ECC 미지원, throttle reason 일부 미발생 등으로 일부 cause 의 base score 가 항상 0 인 경우가 많다. 본 환경에서는 0 cause 가 weighting 에서 자연 제외된다
- rise 기반 weight (#244) 는 pod 재시작 직후 약 30분간 baseline 공백 fallback 으로 상시 포화도 rise 로 잡히는 과도기가 있고, 1.5h 이상 지속되는 압박은 baseline 이 따라잡아 weight 가 감쇠한다. 장기 지속 압박은 절대값 기반 `pod:*_score:5m` 과 victim 단위 rule, pressure API 가 계속 노출한다
- victim 단위 rule (#101) 은 절대값 base 를 유지하므로 상시 포화 pod 가 victim dominant 에는 남을 수 있다. cluster dominant 와 victim dominant 가 일시적으로 다른 cause 를 가리킬 수 있다
- LLM 기반 자연어 cause 설명은 본 PR 범위 밖이다. rule-based weighting 까지가 책임이다
