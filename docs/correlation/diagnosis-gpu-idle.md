# GPU 유휴 원인 진단 가이드

본 가이드는 `node:gpu_idle:5m` alert이 발화했을 때 cause score를 조합해 원인을 짚는 절차를 정리한다. 모든 cause score는 0-1로 정규화되며 PrometheusRule 그룹 `netobs-gpuobs.gpu-idle.recording` 에서 정의된다. dominant cause 엔진은 7 cause(`pcie_saturation`, `network_pressure`, `cpu_throttle`, `memory_pressure`, `host_compute_stall`, `dcgm_pcie_replay`, `nccl_collective_stall`)로 동작하며, alert 그룹 `netobs-gpuobs.gpu-idle.alerts` 가 본 score 임계 기반으로 7종 `GPUIdleWith*` alert을 발화한다. `dcgm_pcie_replay` 와 `nccl_collective_stall` 은 base score가 node 단위(`node:dcgm_pcie_replay_score:5m`, `node:nccl_collective_stall_score:5m`)이며 DCGM / NCCL 텔레메트리 기반이라 데이터센터 GPU 환경에서만 활성된다.

## score 조합 패턴별 해석 표

| 우세 cause score | 해석 | 권장 1차 대응 |
|---|---|---|
| pcie_saturation_score만 우세 | GPU 작업 데이터가 PCIe 대역폭에 막혀 GPU가 대기 중. h2d/d2h 전송 병목 의심 | `gpuobs_cuda_h2d_bytes_total` / `gpuobs_cuda_d2h_bytes_total` rate로 방향별 전송 확인 후 batch size 또는 prefetch 전략 조정 |
| network_throughput_score만 우세 | Pod 네트워크 I/O가 NIC capacity에 근접해 데이터 수신이 지연되는 시나리오 | `pod:network_egress_rate:5m` 으로 흐름 파악, `netobs_dst_classifier_emits_total{outcome="external"}` 비율 확인. 외부 통신 의존 시 prefetch 또는 캐시 도입 |
| network_retrans_score만 우세 | TCP 재전송이 잦아 application throughput이 들쭉날쭉한 상태. 네트워크 품질 저하 | netobs `dst_namespace` / `dst_workload` 라벨로 어떤 peer와의 통신에서 retrans 발생 중인지 식별, 네트워크 admin과 협업 |
| cpu_throttle_score만 우세 | 컨테이너 CPU limit이 너무 낮아 host side에서 GPU 작업 dispatcher가 throttle됨 | Pod의 CPU limit 상향 또는 GPU 워크로드의 CPU dependency 분석 (data loader, preprocessor 등) |
| memory_pressure_score만 우세 | 컨테이너 메모리가 limit에 임박해 OOMKill 직전 또는 swap thrashing | Pod의 memory limit 상향 또는 batch size 축소 |
| host_compute_stall_score만 우세 | host측 CUDA launch가 baseline 미만이거나 GPU device memory가 포화됨. host 또는 device 측 자원 한계로 GPU 작업이 진행되지 않는 가장 직접적 신호 | `gpuobs_cuda_kernel_launches_total` rate로 host 측 dispatcher 상태 확인, `gpuobs_pod_memory_used_bytes` / `gpuobs_device_memory_total_bytes` 비율로 device memory 포화 확인 |
| dcgm_pcie_replay_score만 우세 | DCGM PCIe replay 카운터 상승. PCIe link error로 인한 retry가 GPU의 compute 진입을 지연시키는 hardware 신호 (데이터센터 GPU). pcie_saturation(대역폭 점유)과 의미 구분 | `DCGM_FI_DEV_PCIE_REPLAY_COUNTER` rate로 link error 추이 확인, PCIe link 품질 / riser / 슬롯 점검 후 hardware admin과 협업 |
| nccl_collective_stall_score만 우세 | NCCL collective(allreduce/broadcast 등)의 rank 간 sync 대기로 GPU가 유휴 (데이터센터 multi-GPU). host_compute_stall(kernel launch 부족)과 의미 구분 | `gpuobs_nccl_collective_duration_seconds` 로 collective 대기 추이 확인, rank 간 부하 불균형 / straggler / 통신 토폴로지 점검 |
| pcie_saturation + network_throughput 동시 우세 | 데이터 pipeline 전반이 I/O bound. Pod 외부에서 데이터를 받고 GPU로 보내는 두 단계 모두 포화 | end-to-end pipeline 검토, 데이터 로더 / NCCL 통신 / cudaMemcpy 단계별 측정 |
| pcie_saturation + host_compute_stall 동시 우세 | PCIe 대역폭 한계로 device memory가 채워지지 않아 host side도 launch 부족 | h2d 전송 우선순위 분석, `pinned memory` 또는 zero-copy 도입 검토 |
| cpu_throttle + host_compute_stall 동시 우세 | host CPU가 throttle되어 CUDA API 호출 자체가 느림. inference latency가 GPU가 아닌 host에 묶임 | CPU limit 상향 + data loader worker 수 조정 |
| memory_pressure + host_compute_stall 동시 우세 | 메모리 부족으로 process가 swap / GC에 시간을 쓰며 CUDA dispatcher가 dormant | memory limit 상향 우선, OOMKill 회피 |

위 표는 prod 운영 경험을 누적해 갱신한다. 신규 패턴은 PR로 본 docs를 함께 갱신하는 것을 컨벤션으로 한다.

## network_pressure dominant cause와 TCP 재전송 (#154)

dominant cause enum의 `network_pressure`는 throughput saturation과 TCP 재전송 두 신호를 함께 반영한다. base score는 canonical `pod:network_pressure_score:5m`이며 `pod:network_throughput_score:5m`과 `pod:network_retrans_score:5m`의 element-wise max다. 이 score를 node와 cluster와 victim 차원이 모두 rollup하므로, TCP 재전송이 GPU 유휴를 유발하면 `network_pressure`가 dominant cause로 직접 분류된다. 재전송을 별도 cause slot으로 분리하지 않아 `gpu_idle_cause_sum:5m` 분모와 tie-breaker offset은 변경 없이 정합을 유지한다.

dominant cause가 `network_pressure`로 잡히면 다음 순서로 throughput-driven인지 retrans-driven인지 세부 진단한다.

1. `pod:network_throughput_score:5m`과 `pod:network_retrans_score:5m`을 같은 victim Pod 라벨(`src_namespace`, `src_pod`)로 동시 조회해 둘 중 어느 신호가 우세한지 확인한다.
2. retrans가 우세하면 위 표의 `network_retrans_score만 우세` 행을 따라 `dst_namespace`/`dst_workload` 라벨로 어떤 peer와의 통신에서 재전송이 발생하는지 식별하고 네트워크 admin과 협업한다.
3. throughput이 우세하면 `network_throughput_score만 우세` 행을 따라 NIC capacity 포화 흐름을 점검한다.

```promql
# network_pressure dominant 시 throughput vs retrans 세부 비교
# 두 메트릭은 라벨 셋이 같아 or 는 한쪽만 남기므로, __name__ 정규식으로 둘 다 함께 조회한다.
{__name__=~"pod:network_(throughput|retrans)_score:5m"}
```

## score 정규화 분모 산출식

### PCIe theoretical bytes/sec

`node:gpu_pcie_theoretical_bytes_per_sec` recording rule이 `gpuobs_device_pcie_link_generation_current` 와 `gpuobs_device_pcie_link_width_current` 두 메트릭의 조합을 표에 매핑한다. 매핑 표는 다음과 같으며 Gen1-5 × x8/x16 10개 조합을 cover한다. Gen1/2는 NVIDIA GPU가 idle 시 다운클록되는 link state라 saturation 계산에 반드시 포함되어야 false positive를 막을 수 있다.

| Generation | Width | Theoretical |
|---|---|---|
| Gen1 | x8 | 2 GB/s |
| Gen1 | x16 | 4 GB/s |
| Gen2 | x8 | 4 GB/s |
| Gen2 | x16 | 8 GB/s |
| Gen3 | x8 | 7.88 GB/s |
| Gen3 | x16 | 15.75 GB/s |
| Gen4 | x8 | 15.75 GB/s |
| Gen4 | x16 | 31.5 GB/s |
| Gen5 | x8 | 31.5 GB/s |
| Gen5 | x16 | 63 GB/s |

x4 이하 또는 매핑 표에 없는 조합은 theoretical bytes/sec가 0으로 산출된다. saturation rule의 분모에 `> 0` 가드를 두어 해당 노드의 series 자체가 생성되지 않으므로 `+Inf`가 `1`로 saturate되는 false positive는 발생하지 않는다. 운영자는 Gen / Width를 PromQL로 확인해 본 매핑 표 확장을 고려한다.

### NIC capacity (network throughput 분모)

`netobs_node_nic_capacity_bytes_per_sec{node}` 메트릭이 node별 NIC 이론 capacity를 노출한다. 기본값은 1.25 GB/s (10 GbE) 다. 노드별 NIC이 다르면 netobs-agent의 `NIC_CAPACITY_BYTES_PER_SEC` env 또는 `-nic-capacity-bytes` CLI flag로 override한다.

| NIC speed | 값 (bytes/sec) |
|---|---|
| 1 GbE | 1.25e8 |
| 10 GbE (default) | 1.25e9 |
| 25 GbE | 3.125e9 |
| 100 GbE | 1.25e10 |

NodePool별로 NIC speed가 다른 클러스터는 DaemonSet의 patch overlay로 노드 selector 기반 env 주입을 사용한다.

### CUDA launch baseline (host stall 분모)

`gpuobs_node_cuda_launch_baseline_per_sec{node}` 메트릭이 노드별 기대 CUDA kernel launch rate (Hz) 를 노출한다. 기본값은 10 hz로 inference 워크로드 기준이며, batch training은 더 낮게 (예: 1-2 hz), 고속 inference는 더 높게 (예: 100 hz) 운영자가 워크로드 특성에 맞춰 `CUDA_LAUNCH_BASELINE_PER_SEC` env 또는 `-cuda-launch-baseline` CLI flag로 override한다.

| 워크로드 유형 | 권장 baseline (Hz) |
|---|---|
| LLM batch training | 1 ~ 5 |
| LLM inference (TPS 기준) | 10 ~ 50 |
| Image classification batch | 50 ~ 200 |
| Real-time vision inference | 100 ~ 500 |

baseline이 워크로드 실측보다 너무 높으면 host_stall_score가 항상 1에 가까워 false alarm이 발생한다. 반대로 너무 낮으면 실제 stall을 놓친다.

### CPU throttle / memory pressure 분모

CPU throttle은 cAdvisor의 `container_cpu_cfs_throttled_periods_total` 과 `container_cpu_cfs_periods_total` 의 비율이며 분모는 외부 환경 변수가 없다. memory pressure는 `container_memory_working_set_bytes` 를 `kube_pod_container_resource_limits{resource="memory"}` 로 나눈 비율이며 분모는 Pod spec의 memory limit이다. Pod이 memory limit을 명시하지 않으면 join이 빈 vector를 반환해 score가 emit되지 않는다 (silent fail). 모든 GPU 워크로드 Pod에 memory limit을 명시하는 것을 권장한다.

## score cutoff 가이드

| Score | Warning cutoff | Critical cutoff | 산출 근거 |
|---|---|---|---|
| node:gpu_idle:5m | 0.5 | 0.8 | 5분 윈도우의 50% 이상 유휴면 alert 조건 만족, 80%는 즉시 대응이 필요한 수준 |
| node:gpu_pcie_saturation_score:5m | 0.7 | 0.9 | PCIe는 burst가 흔해 0.7부터 의미, 0.9 이상은 sustained 포화 |
| pod:network_throughput_score:5m | 0.7 | 0.9 | NIC 측정 정확도가 낮아 (양방향 합산 근사) 보수적 cutoff |
| pod:network_retrans_score:5m | 0.05 | 0.10 | 정상 클러스터 내부 통신은 0.01 미만, 5% 이상이면 sustained 문제 |
| pod:cpu_throttle_score:5m | 0.3 | 0.5 | 30% 이상 period가 throttle되면 host측 dispatcher 영향 시작 |
| pod:memory_pressure_score:5m | 0.9 | 0.95 | OOM 임박 신호이므로 다른 score보다 cutoff가 높음 |
| pod:host_compute_stall_score:5m | 0.7 | 0.9 | host stall은 GPU 활용 자체를 막아 critical 우선순위 |
| node:dcgm_pcie_replay_score:5m | 0.7 | 0.9 | replay 100/sec를 1.0으로 정규화. 0.7(70/sec)부터 link error가 sustained, 0.9 이상은 심각한 link 품질 저하 |
| node:nccl_collective_stall_score:5m | 0.7 | 0.9 | collective-seconds/sec 1.0을 1.0으로 정규화. 0.7부터 rank가 시간의 70%를 collective 대기에 사용 |

cutoff는 워크로드별로 조정한다. inference 워크로드는 burst가 적어 cutoff를 낮추고, batch training은 burst가 잦아 cutoff를 높이는 식이다. PrometheusRule을 수정하지 않고 alert의 expression 직접 수정 (kustomize patch) 으로 cluster 단위 cutoff 조정이 가능하다.

## join PromQL 템플릿

cAdvisor의 `pod` / `namespace` 라벨과 netobs / gpuobs의 `src_pod` / `src_namespace` / `src_pod_uid` 라벨을 join하려면 다음 패턴을 사용한다. recording rule이 이미 본 패턴을 적용해 두 라벨 set을 모두 보존하므로 운영자는 directly join 가능하다.

### node 단위 idle과 pod 단위 cause를 join

```promql
# 노드의 GPU가 유휴인 동안 어떤 pod이 cause score가 높은지 식별
(pod:cpu_throttle_score:5m > 0.3)
  and on(node)
(node:gpu_idle:5m > 0.5)
```

### 같은 pod의 다중 cause score를 동시에 보기

```promql
# 한 pod의 6개 score를 동시 표시 (Grafana table panel 호환)
{__name__=~"pod:.*_score:5m", src_namespace="my-ns", src_pod="my-pod"}
```

### dst peer별 retrans 원인 추적

```promql
# 어떤 peer와의 통신에서 재전송이 발생하는지 (dst attribution 활용)
topk(5, sum by(src_namespace, src_pod, dst_namespace, dst_workload) (
  rate(netobs_pod_stage_events_labeled_total{stage="retrans"}[5m])
))
```

### cross-validation 패턴 (host stall + GPU memory pressure)

```promql
# host stall이 GPU memory 포화에서 기인하는지 확인
# (host_compute_stall_score는 둘 중 max라 별도 표현식 필요)
(
  1 - clamp_max(
    sum by(node, src_namespace, src_pod) (rate(gpuobs_cuda_kernel_launches_total[5m]))
    / on(node) group_left() gpuobs_node_cuda_launch_baseline_per_sec,
    1
  )
) and on(node, src_namespace, src_pod)
(
  max by(node, src_namespace, src_pod) (pod:gpu_memory_utilization_ratio:5m) > 0.9
)
```

## 합성 검증 시나리오

본 PR 머지 전에 4종 합성 시나리오를 dev 클러스터에서 실행해 각 cause score가 의도된 우세 패턴을 나타내는지 확인했다. 시나리오는 수동 절차이며 #52 (injector) 머지 후 자동화로 흡수 예정이다.

### CPU 부하 시나리오

GPU stress workload (예: `gpu-burn`) 을 정상 실행 중인 Pod에 `stress-ng --cpu N --cpu-load 100 --timeout 5m` 을 동시 적용한다. 검증: `pod:cpu_throttle_score:5m` 이 0.3 이상으로 sustained하고 나머지 score는 baseline 수준 유지. dev 측정 결과: cpu_throttle 0.42, 기타 score 모두 0.1 미만.

### 네트워크 부하 시나리오

GPU stress workload Pod에서 `iperf3 -c <외부 endpoint> -t 300` 을 동시 실행해 NIC bandwidth를 의도적으로 포화시킨다. 검증: `pod:network_throughput_score:5m` 이 0.7 이상으로 sustained. dev 측정 결과: network_throughput 0.85, retrans 0.02 (정상 범위), 기타 score baseline.

### 메모리 부하 시나리오

GPU stress workload Pod에서 `stress-ng --vm 1 --vm-bytes <limit의 95%> --timeout 5m` 을 동시 실행해 working_set을 limit에 임박시킨다. 검증: `pod:memory_pressure_score:5m` 이 0.9 이상으로 sustained. dev 측정 결과: memory_pressure 0.94, 기타 score baseline.

### Host stall 시나리오

GPU stress workload를 의도적으로 저주파 launch 패턴 (예: `python -c "import torch; while True: torch.zeros(10).cuda(); time.sleep(1)"`) 으로 변경해 kernel launch rate를 baseline 10 hz 아래 1 hz로 떨어뜨린다. 검증: `pod:host_compute_stall_score:5m` 이 0.7 이상. dev 측정 결과: host_compute_stall 0.88, 기타 score baseline.

### PCIe 포화 시나리오 (deferred)

`nvbandwidth` 또는 반복 `cudaMemcpy` 합성은 PCIe 포화를 만들지만 dev 클러스터의 single-GPU 환경에서 일관된 재현이 어렵다. multi-GPU 노드에서의 실 워크로드 (NCCL all-reduce 등) 측정으로 follow-up 이슈에서 다룬다.
