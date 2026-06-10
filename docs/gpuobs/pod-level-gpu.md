# Pod-level GPU utilization 운영 가이드 (#104)

본 문서는 gpuobs-agent 가 #104 에서 도입한 Pod-level GPU compute utilization 메트릭 (`gpuobs_pod_utilization_percent`) 의 운영적 의미와 환경별 동작, 그리고 troubleshooting 절차를 정리한다. 기존 device-level (`gpuobs_device_utilization_percent`) 와 Pod-level 메모리 (`gpuobs_pod_memory_used_bytes`) 가 정상 동작 중인 환경을 전제한다.

## 신규 메트릭 셋

본 이슈로 추가된 메트릭은 5종이다.

- `gpuobs_pod_utilization_percent{node, src_namespace, src_pod, src_pod_uid, gpu_uuid, gpu_index}`: Pod 단위 GPU compute utilization (0-100). NVML `DeviceGetProcessUtilization` 결과를 cgroup 기반 PID 매핑으로 (Pod, GPU) 단위 합산 (100 cap) 한 값. 라벨 셋은 기존 `gpuobs_pod_memory_used_bytes` 와 정합 유지되어 PromQL join 호환.
- `gpuobs_pod_mig_utilization_percent{..., mig_uuid, gi_id}`: MIG 활성 환경 한정의 instance 단위 Pod utilization. `mig_uuid` 와 `gi_id` (GPU Instance ID) 라벨이 추가 부착되어 동일 device 내 instance 간 분리. parent device 의 MIG mode 가 `enabled` 일 때만 시리즈가 생긴다.
- `gpuobs_mig_mode{node, gpu_uuid, gpu_index, gpu_model, mode}`: device 단위 MIG 활성 상태. `mode` 라벨은 `enabled` / `disabled` / `unsupported` 3종이며 각 device 마다 3 시리즈 모두 0/1 로 발행되어 라벨 전환 시 stale 시리즈가 남지 않는다.
- `gpuobs_mps_active{node, gpu_uuid, gpu_index, gpu_model}`: MPS daemon active 여부 (1=active, 0=inactive). `CUDA_MPS_PIPE_DIRECTORY` env / 기본 경로 / `nvidia-cuda-mps-control` process 3종 OR 로직으로 노드 단위 감지.
- `pod:gpu_util_p95:5m{node, src_namespace, src_pod, src_pod_uid, gpu_uuid}`: 위 Pod-level utilization 의 5분 윈도우 95-percentile recording rule. capacity-trends (#86 과 #88) 의 GPU 도메인 device-scope fallback 강등이 본 rule 활성화로 회복된다. #120 의 `pod:gpu_network_correlation_score:5m` 와의 4 Pod 라벨 join key 정합 을 위해 `node` 와 `src_pod_uid` 라벨도 함께 보존한다.

## 환경별 동작 매트릭스

`gpuobs_mig_mode` 와 `gpuobs_mps_active` 의 조합으로 Pod util 메트릭의 정확도 환경을 즉시 파악할 수 있다.

- `mig_mode=enabled` + `mps_active=0`: 가장 정확한 환경. Pod util 은 `gpuobs_pod_mig_utilization_percent` 에 instance 단위로 분리 발행되며 process attribution 정확.
- `mig_mode=disabled` + `mps_active=0`: 표준 데이터센터 GPU 환경. Pod util 은 `gpuobs_pod_utilization_percent` 에 device 단위로 발행. nn.DataParallel 또는 model parallelism workload 에서 `gpuobs_cuda_pid_multi_gpu_count` 가 nonzero 면 attribution 이 best-effort.
- `mig_mode=unsupported` + `mps_active=0`: consumer GPU (RTX 3090 등) 또는 MIG 미지원 데이터센터 GPU 환경 (dev cluster 기본). Pod util 은 동일 메트릭으로 발행되되 graceful degradation 경로. process util 의 정확도는 NVML `GetProcessUtilization` 의 single-process 점유 시나리오에서 가장 신뢰 가능.
- 임의 환경 + `mps_active=1`: MPS daemon 이 단일 CUDA context 를 다수 process 로 time-slice 하는 환경. NVML 이 per-process 분할 정보를 노출하지 않아 Pod util 정확도가 떨어진다. dashboard annotation 으로 자동 노출되며 운영자는 본 신호 hit 시 절대 수치 해석을 피한다.

## MIG enable / disable 절차

데이터센터 GPU (A100 / H100) 에서 MIG 를 활성화 하려면 device 별로 다음 명령 실행 후 driver reset 필요.

```sh
# MIG 모드 활성화 (device 0 기준)
nvidia-smi -i 0 -mig 1

# GPU Instance 생성 (예시: profile id 19 의 1g.10gb 4개)
nvidia-smi mig -i 0 -cgi 19,19,19,19

# Compute Instance 생성 (각 GPU Instance 의 모든 슬롯)
nvidia-smi mig -i 0 -cci

# 활성화 확인
nvidia-smi mig -i 0 -lgi
```

Kubernetes 노드에서는 nvidia-device-plugin 의 MIG strategy 설정도 필요하다. `mig.strategy=single` 은 device-plugin 이 단일 profile 의 instance 만 advertise 하고, `mig.strategy=mixed` 는 여러 profile 을 함께 노출한다. mixed 환경에서는 `nvidia.com/mig-1g.10gb` 같이 profile 별 resource name 이 노드 라벨로 표시된다.

본 agent 는 device-plugin 설정과 무관하게 NVML 단에서 instance 를 enumerate 하므로 strategy 변경 후 agent 재시작은 불필요하다. MIG mode 변경 후에는 NVML 핸들 캐싱 정합 위해 gpuobs-agent Pod 1회 재기동을 권장한다.

## dev cluster (RTX 3090) 의 graceful degradation 동작

dev cluster 의 RTX 3090 단일 GPU 는 MIG 미지원이라 본 PR 의 검증은 graceful degradation 경로만 cover 한다.

- `gpuobs_mig_mode{mode="unsupported"} == 1`: 1 시리즈 발행, 다른 mode 라벨은 0.
- `gpuobs_mps_active == 0`: MPS daemon 미실행 환경.
- `gpuobs_pod_utilization_percent`: active CUDA workload Pod 에 대해 NVML `GetProcessUtilization` 기반으로 정상 emit.
- `gpuobs_pod_mig_utilization_percent`: 시리즈 자체가 생기지 않음 (MIG 미활성).
- `pod:gpu_util_p95:5m`: workload 가 5분 이상 가동되면 산출 시작.

## 카디널리티 통제

신규 util 메트릭은 라벨 6-8종이라 클러스터 규모가 클 때 카디널리티 폭증 위험이 있다. 다음 env 와 flag 로 발행 범위를 좁힐 수 있다.

- `GPUOBS_POD_UTIL_ALLOW_NAMESPACES` env / `-pod-util-allow-namespaces` flag: 콤마 구분 namespace 화이트리스트. 빈 값이면 전체 namespace 발행, 명시 시 매칭 namespace 만 emit. 본 통제는 신규 util 메트릭에만 적용되며 기존 `gpuobs_pod_memory_used_bytes` 발행 정책에는 영향 없음.
- `GPUOBS_POD_METRICS_ENABLED=false`: 기존 PodMetricsEnabled flag. true 가 기본값이며 false 일 때 `gpuobs_pod_*` 전체 (memory + util) 가 비활성화.

두 layer 는 독립적이라 PodMetricsEnabled 활성 + allow-list 부분 적용 조합이 정상 동작한다.

## Troubleshooting

- 메트릭 미emit 인 경우 우선 `gpuobs_mig_mode` 시리즈가 노출되는지 확인. 시리즈 부재면 nvml init 실패 가능성이 크고 (`/var/log` 또는 Pod log 에서 `gpuobs: nvml init` 검색), 시리즈가 있는데 `gpuobs_pod_utilization_percent` 만 빈 경우 active CUDA workload 부재 또는 allow-list 미매칭 확인.
- `gpuobs_mig_mode{mode="unsupported"}` 가 데이터센터 GPU (A100/H100) 에서 노출되는 경우 MIG 미활성 상태이거나 driver 가 MIG capable 모드로 로드되지 않은 경우. `nvidia-smi -q | grep -A 2 MIG` 로 capability 확인 후 위 절차로 활성화.
- `gpuobs_cuda_pid_multi_gpu_count > 0` 일 때 `gpuobs_pod_utilization_percent` 값이 흔들리는 경우 nn.DataParallel 또는 model parallelism workload 가 단일 process 로 multi-GPU 점유 중인 신호. MIG 분할 환경에서는 process 가 단일 instance 에 bind 되어 본 문제 자연 해소.
- `gpuobs_mps_active == 1` 환경에서 절대 수치 해석을 피하고 추세 비교만 활용. MPS 분할 정밀화는 본 이슈 비목표.
- time-slicing (nvidia-device-plugin `sharing.timeSlicing`) 환경의 Pod util 은 NVML sampling window 가 time-slice 경계와 정렬 보장이 없어 정확한 wall-clock 분할이 안 된다. 본 환경의 정밀 측정도 본 이슈 비목표.
