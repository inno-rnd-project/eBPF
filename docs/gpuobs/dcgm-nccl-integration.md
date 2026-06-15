# NVIDIA DCGM과 NCCL profiling 인터페이스 통합

## 배경

PR #79의 dominant cause classification은 5 cause weight (`pcie_saturation`와 `network_pressure`와 `cpu_throttle`와 `memory_pressure`와 `host_compute_stall`) 의 heuristic score 조합으로 GPU 유휴 원인을 노출하지만 hardware-level 직접 측정의 신뢰도가 일부 떨어진다. NVIDIA DCGM의 PCIe replay count와 NVLink throughput 같은 hardware counter와 NCCL profiler의 collective wait 신호 통합이 정밀도 향상에 필요하지만 dev cluster의 RTX 3090 단일 GPU 환경에서는 DCGM 일부 메트릭과 NCCL collective 자체의 실 가치 검증이 불가능하다. 본 PR (#123) 은 인터페이스 수준의 skeleton만 신설하고 실증은 데이터센터 GPU (A100과 H100 등) 환경 확보 시점으로 위임한다.

## skeleton 구조

본 PR은 NVML과 동등 레벨의 leaf 패키지 2종을 신설한다.

### `internal/gpuobs/dcgm/`

`Source` 인터페이스가 DCGM 메트릭 fetch의 추상 진입점이다. 메서드 셋은 `Available`와 `MetricForward(prefix)`와 `Close` 3종이고 production 구현은 build tag (`//go:build dcgm`) 분리한 파일에서 NVIDIA DCGM SDK 또는 dcgm-exporter HTTP endpoint를 호출한다. 기본 구현 `noopSource`는 `NewNoop()` factory로 생성되며 모든 메서드가 graceful empty 결과를 돌려준다.

`Sample` struct는 단일 sample의 4 필드 (`Name`와 `Labels`와 `Value`와 `Timestamp`) 를 묶는다. cardinality 가드는 `Labels`의 키 셋을 device와 gpu_uuid 같은 폐쇄 enum으로 한정해 보장한다.

### `internal/gpuobs/nccl/`

`Profiler` 인터페이스가 NCCL collective event 비동기 수집의 추상 진입점이다. 메서드 셋은 `Available`와 `Attach`와 `Events() <-chan Event`와 `Close` 4종이고 production 구현은 build tag (`//go:build nccl`) 분리한 파일에서 NCCL profiler callback 또는 cuProfiler symbol을 attach한다. 기본 구현 `noopProfiler`는 `Events()` 가 미리 close된 channel을 돌려주어 호출 자의 range 루프가 정상 종료한다.

`Event` struct는 collective operation 단일 sample의 4 필드 (`Operation`와 `DurationNs`와 `RankCount`와 `Timestamp`) 를 묶는다.

## RTX 3090 환경의 graceful degradation 동작

dev cluster의 RTX 3090 단일 GPU 환경에서는 다음 흐름이 default다.

- `cmd/gpuobs-agent/main.go`가 `dcgm.NewNoop()`와 `nccl.NewNoop()` 인스턴스를 생성
- 두 인스턴스의 `Available()` 결과가 false라 `gpuobs_dcgm_available=0`와 `gpuobs_nccl_profiler_available=0` emit
- recording rule `gpu_idle_cause_weight:5m{cause="nccl_collective_stall"}`와 `gpu_idle_cause_weight:5m{cause="dcgm_pcie_replay"}` 가 idle 게이팅 활성 시간대에 0 weight emit
- `cluster:gpu_idle_dominant_cause:5m`의 topk 선택은 기존 5 cause weight 중에서만 이루어지므로 dominant cause 분류 결과 회귀 zero

`GPUOBS_DCGM_ENABLED=true` 또는 `GPUOBS_NCCL_ENABLED=true` env를 명시해도 본 PR의 wire-up 흐름은 SDK 통합 부재를 warn log로 안내하고 noop을 유지한다. 실제 SDK 통합 분기는 별도 follow-up PR의 build tag 또는 runtime dlopen에서 도입한다.

## 데이터센터 GPU 환경의 활성 절차

A100 또는 H100 같은 데이터센터 GPU 환경 확보 후 다음 follow-up 단계를 거친다.

- DCGM SDK 또는 dcgm-exporter sidecar 배포 후 `internal/gpuobs/dcgm/dcgm_real.go` (build tag `//go:build dcgm`) 신설로 production `Source` 구현 도입
- NCCL profiler callback 또는 cuProfiler symbol attach를 `internal/gpuobs/nccl/nccl_real.go` (build tag `//go:build nccl`) 에서 도입
- `GPUOBS_DCGM_ENABLED=true`와 `GPUOBS_NCCL_ENABLED=true` opt-in 토글 활성과 함께 build tag 적용한 image를 별도 빌드
- recording rule의 `nccl_collective_stall`와 `dcgm_pcie_replay` weight 산출 식을 vector(0) 에서 실제 base score로 교체

## 기존 5 cause와 신규 슬롯의 의미 분리

신규 슬롯이 기존 5 cause와 의미 중복되지 않도록 다음 표로 책임 영역을 분리한다.

| Cause | 측정 신호 | 입력 메트릭 |
|---|---|---|
| `pcie_saturation` | heuristic PCIe 대역폭 점유율 | `node:gpu_pcie_saturation_score:5m` |
| `network_pressure` | Pod egress와 ingress의 NIC capacity 점유율 | `pod:network_throughput_score:5m`와 `pod:network_retrans_score:5m` |
| `cpu_throttle` | CFS throttle 비율 | `pod:cpu_throttle_score:5m` |
| `memory_pressure` | working_set과 limit 비율 | `pod:memory_pressure_score:5m` |
| `host_compute_stall` | CUDA kernel launch rate 저하 | `pod:host_compute_stall_score:5m` |
| `nccl_collective_stall` (#123 신규) | NCCL allreduce와 broadcast의 rank wait 신호 | `nccl.Profiler.Events` (데이터센터 GPU 환경 활성 시) |
| `dcgm_pcie_replay` (#123 신규) | DCGM PCIe replay count hardware counter | `dcgm.Source.MetricForward("dcgm_pcie:")` (데이터센터 GPU 환경 활성 시) |

`host_compute_stall`은 host 측 launch 부족 신호이고 `nccl_collective_stall`은 rank 간 sync wait 신호라 별개 원인이다. `pcie_saturation`은 heuristic 대역폭 점유 신호이고 `dcgm_pcie_replay`는 hardware-level PCIe 링크 에러 신호라 별개 원인이다.

## 비목표

- RTX 3090 외 GPU 모델 (A100, H100 등) 의 실증 검증은 본 PR 외 (데이터센터 GPU environment matrix 확보 후 follow-up)
- DCGM exporter의 helm 설치 자동화는 본 PR 외
- NCCL collective 자체의 부하 시뮬레이션은 본 PR 외
- AI 기반 dominant cause classification 자동 학습은 본 PR 외
- 신규 cause 슬롯의 weight 계산 식 도입은 본 PR 외 (기본값 0 emit만 도입)
- `internal/gpuobs/nvml/`의 `Device` 인터페이스 변경은 본 PR 외
- dashboard 신규 panel 추가는 본 PR 외
