# NVIDIA DCGM과 NCCL profiling 인터페이스 통합

## 배경

PR #79의 dominant cause classification은 5 cause weight (`pcie_saturation`와 `network_pressure`와 `cpu_throttle`와 `memory_pressure`와 `host_compute_stall`) 의 heuristic score 조합으로 GPU 유휴 원인을 노출하지만 hardware-level 직접 측정의 신뢰도가 일부 떨어진다. NVIDIA DCGM의 PCIe replay count와 NVLink throughput 같은 hardware counter와 NCCL profiler의 collective wait 신호 통합이 정밀도 향상에 필요하지만 dev cluster의 RTX 3090 단일 GPU 환경에서는 DCGM 일부 메트릭과 NCCL collective 자체의 실 가치 검증이 불가능하다. PR #123은 인터페이스 수준의 skeleton만 신설했고 이후 #133이 DCGM을 dcgm-exporter HTTP 방식으로 활성했으며 #134가 NCCL profiler를 libnccl.so uprobe 방식으로 활성한다. 두 신규 cause의 실증은 데이터센터 GPU (A100과 H100 등) 환경 확보 시점으로 위임하되 production 구현과 recording rule과 빌드 정합은 본 PR에서 완비한다.

## skeleton 구조

본 PR은 NVML과 동등 레벨의 leaf 패키지 2종을 신설한다.

### `internal/gpuobs/dcgm/`

`Source` 인터페이스가 DCGM 메트릭 fetch의 추상 진입점이다. 메서드 셋은 `Available`와 `MetricForward(prefix)`와 `Close` 3종이고 production 구현은 build tag (`//go:build dcgm`) 분리한 파일에서 NVIDIA DCGM SDK 또는 dcgm-exporter HTTP endpoint를 호출한다. 기본 구현 `noopSource`는 `NewNoop()` factory로 생성되며 모든 메서드가 graceful empty 결과를 돌려준다.

`Sample` struct는 단일 sample의 4 필드 (`Name`와 `Labels`와 `Value`와 `Timestamp`) 를 묶는다. cardinality 가드는 `Labels`의 키 셋을 device와 gpu_uuid 같은 폐쇄 enum으로 한정해 보장한다.

### `internal/gpuobs/nccl/`

`Profiler` 인터페이스가 NCCL collective event 비동기 수집의 추상 진입점이다. 메서드 셋은 `Available`와 `Attach`와 `Events() <-chan Event`와 `Close` 4종이다. production 구현 `productionProfiler` (`nccl_real.go`, build tag `//go:build nccl`) 는 `libnccl.so.2`의 collective 심볼 (`ncclAllReduce`와 `ncclBroadcast`와 `ncclReduceScatter`와 `ncclAllGather`) 에 uprobe와 uretprobe를 attach해 collective의 entry-exit wall-clock을 ringbuf로 수집하고 `Events()` 채널로 emit한다. attach mechanism은 `internal/gpuobs/cuda`와 동일하게 uprobe_multi link를 써 `perf_event_paranoid` 정책을 우회한다. build tag `nccl`이 비활성인 기본 빌드에서는 `nccl_stub.go`의 `NewProduction`이 noop을 돌려주고 `noopProfiler`의 `Events()` 가 미리 close된 channel을 돌려주어 호출자의 range 루프가 정상 종료한다.

`Event` struct는 collective operation 단일 sample의 4 필드 (`Operation`와 `DurationNs`와 `RankCount`와 `Timestamp`) 를 묶는다. NCCL의 `ncclComm` 핸들은 opaque internal struct라 rank count를 BTF로 추출 불가하므로 `RankCount`는 best-effort로 0을 두고 collective의 wall-clock (`DurationNs`) 만 측정한다.

## RTX 3090 환경의 graceful degradation 동작

dev cluster의 RTX 3090 단일 GPU 환경에서는 다음 흐름이 default다.

- `cmd/gpuobs-agent/main.go`가 `dcgm.NewNoop()`와 `nccl.NewNoop()` 인스턴스를 생성
- 두 인스턴스의 `Available()` 결과가 false라 `gpuobs_dcgm_available=0`와 `gpuobs_nccl_profiler_available=0` emit
- recording rule `gpu_idle_cause_weight:5m{cause="nccl_collective_stall"}`와 `gpu_idle_cause_weight:5m{cause="dcgm_pcie_replay"}` 가 base 메트릭 부재로 `or vector(0)` fallback을 거쳐 idle 게이팅 활성 시간대에 0 weight emit
- `cluster:gpu_idle_dominant_cause:5m`의 topk 선택은 사실상 기존 5 cause weight 중에서만 이루어지므로 dominant cause 분류 결과 회귀 zero

기본 이미지는 build tag `nccl`이 비활성이라 `GPUOBS_NCCL_ENABLED=true`를 명시해도 `nccl.NewProduction`이 stub noop을 돌려주고 `Available()`가 false라 graceful degradation을 유지한다. NCCL production attach는 build tag `nccl`로 빌드한 이미지에서만 활성된다.

## DCGM 활성 절차 (#133, dcgm-exporter HTTP 방식)

A100 또는 H100 같은 데이터센터 GPU 환경에서 DCGM 통합은 dcgm-exporter HTTP endpoint 방식으로 활성한다. `internal/gpuobs/dcgm/http_source.go`의 production `Source`가 dcgm-exporter의 `/metrics`를 순수 Go HTTP client로 fetch하므로 CGO와 libdcgm.so 의존과 build tag 분리가 모두 불요하다.

- NVIDIA GPU Operator 또는 standalone manifest로 dcgm-exporter를 배포해 `DCGM_FI_DEV_PCIE_REPLAY_COUNTER`와 `DCGM_FI_DEV_NVLINK_BANDWIDTH_*` 같은 hardware counter를 Prometheus에 emit
- gpuobs-agent에 `GPUOBS_DCGM_ENABLED=true` env (또는 `-dcgm` flag) 를 설정해 `dcgm.NewHTTPSource` wire-up을 활성. dcgm-exporter Service 경로가 기본값 (`http://dcgm-exporter.gpu-operator.svc:9400/metrics`) 과 다르면 `GPUOBS_DCGM_EXPORTER_URL` env로 override
- dcgm-exporter가 reachable 하면 `gpuobs_dcgm_available` self-health gauge가 1로 전환
- recording rule `cluster:dcgm_pcie_replay_score:5m`가 `DCGM_FI_DEV_PCIE_REPLAY_COUNTER`의 rate를 정규화해 base score를 산출하고 `gpu_idle_cause_weight:5m{cause="dcgm_pcie_replay"}` weight가 0보다 큰 값으로 활성

## NCCL 활성 절차 (#134, libnccl.so uprobe 방식)

A100 또는 H100 같은 데이터센터 GPU의 multi-rank distributed training (PyTorch DDP와 Megatron 등) 환경에서 NCCL 통합은 libnccl.so uprobe 방식으로 활성한다. `internal/gpuobs/nccl/nccl_real.go`의 production `Profiler`가 cilium/ebpf의 uprobe_multi link와 ringbuf reader에 의존하므로 build tag `nccl`로 빌드한 이미지에서만 컴파일된다.

- `make image-push-all`이 아니라 build tag `nccl`을 넣은 별도 이미지 빌드가 필요하다. `go build -tags nccl`과 bpf2go 산출물의 `//go:build ... && nccl` 태그가 정합해 collective uprobe BPF object가 이미지에 포함된다
- DaemonSet에 host의 `libnccl.so.2`를 hostPath로 마운트한다. 기본 경로는 `/host/usr/lib/x86_64-linux-gnu/libnccl.so.2`이며 다르면 `GPUOBS_NCCL_LIB_PATH` env (또는 `-nccl-lib-path` flag) 로 override한다
- gpuobs-agent에 `GPUOBS_NCCL_ENABLED=true` env (또는 `-nccl-profiler` flag) 를 설정해 `nccl.NewProduction` wire-up을 활성. attach에는 `CAP_BPF`와 `CAP_PERFMON`와 `CAP_SYS_PTRACE`와 kernel 6.6+ (uprobe_multi link) 가 필요하다
- collective 심볼 중 하나라도 attach되면 `gpuobs_nccl_profiler_available` self-health gauge가 1로 전환하고 event 소비 goroutine이 `gpuobs_nccl_collective_duration_seconds{operation}` histogram에 collective wall-clock을 기록한다
- recording rule `cluster:nccl_collective_stall_score:5m`가 histogram `_sum`의 rate를 node 단위로 합산 후 cluster max로 정규화해 base score를 산출하고 `gpu_idle_cause_weight:5m{cause="nccl_collective_stall"}` weight가 0보다 큰 값으로 활성

`RankCount`는 `ncclComm` opaque 제약으로 0으로 두며 collective wall-clock이 base score의 유일한 입력이다. 정규화 임계 (collective-seconds/sec 1.0) 는 노드 GPU 수가 많아 동시 collective 대기 합이 큰 환경에서 운영자가 recording rule의 분모를 GPU 수로 나눠 override 가능하다.

## 기존 5 cause와 신규 슬롯의 의미 분리

신규 슬롯이 기존 5 cause와 의미 중복되지 않도록 다음 표로 책임 영역을 분리한다.

| Cause | 측정 신호 | 입력 메트릭 |
|---|---|---|
| `pcie_saturation` | heuristic PCIe 대역폭 점유율 | `node:gpu_pcie_saturation_score:5m` |
| `network_pressure` | Pod egress와 ingress의 NIC capacity 점유율 | `pod:network_throughput_score:5m`와 `pod:network_retrans_score:5m` |
| `cpu_throttle` | CFS throttle 비율 | `pod:cpu_throttle_score:5m` |
| `memory_pressure` | working_set과 limit 비율 | `pod:memory_pressure_score:5m` |
| `host_compute_stall` | CUDA kernel launch rate 저하 | `pod:host_compute_stall_score:5m` |
| `nccl_collective_stall` (#134 활성) | NCCL collective (allreduce, broadcast 등) 의 rank 간 sync wait 신호 | `cluster:nccl_collective_stall_score:5m` (libnccl.so uprobe의 `gpuobs_nccl_collective_duration_seconds` 기반) |
| `dcgm_pcie_replay` (#133 활성) | DCGM PCIe replay count hardware counter | `cluster:dcgm_pcie_replay_score:5m` (dcgm-exporter의 `DCGM_FI_DEV_PCIE_REPLAY_COUNTER` 기반) |

`host_compute_stall`은 host 측 launch 부족 신호이고 `nccl_collective_stall`은 rank 간 sync wait 신호라 별개 원인이다. `pcie_saturation`은 heuristic 대역폭 점유 신호이고 `dcgm_pcie_replay`는 hardware-level PCIe 링크 에러 신호라 별개 원인이다.

## 비목표

- RTX 3090 외 GPU 모델 (A100, H100 등) 의 실증 검증은 본 PR 외 (데이터센터 GPU environment matrix 확보 후 follow-up)
- DCGM exporter의 helm 설치 자동화는 본 PR 외
- NCCL collective 자체의 부하 시뮬레이션과 multi-rank training 워크로드 기동은 본 PR 외
- build tag `nccl` 이미지의 CI 빌드 파이프라인과 배포 overlay 활성은 본 PR 외 (데이터센터 GPU 환경 확보 후 follow-up)
- AI 기반 dominant cause classification 자동 학습은 본 PR 외
- `internal/gpuobs/nvml/`의 `Device` 인터페이스 변경은 본 PR 외
- dashboard 신규 panel 추가는 본 PR 외
