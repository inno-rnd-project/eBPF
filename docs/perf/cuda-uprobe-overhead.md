# cuda uprobe dispatch hot path overhead 측정

본 문서는 `gpuobs-agent` 의 cuda uprobe 모듈이 PyTorch 등 long-running CUDA 워크로드 환경에서
실제로 추가하는 CPU 비용을 정량 측정한 결과를 기록한다. 측정 워크로드와 매니페스트는
[test/perf/](../../test/perf/) 디렉토리에 분리해 두었으며, 본 문서는 결과 보고서 역할만 한다.

배경 이슈: [#32](https://github.com/inno-rnd-project/eBPF/issues/32)

## 측정 환경

| 항목 | 값 |
|---|---|
| 노드 | dev 클러스터 `gpu` 노드 (RTX 3090, Ubuntu 24.04, kernel 6.8.0-60-generic) |
| GPU 드라이버 | NVML 버전은 측정 시점에 `nvidia-smi --query-gpu=driver_version` 으로 캡처 |
| 에이전트 버전 | 측정 시점의 `gpuobs-agent` VERSION 을 본문에 함께 명시 |
| 워크로드 | [pytorch-resnet50-bench](../../test/perf/pytorch-resnet50-bench.yaml) |
| 워크로드 지속 시간 | warmup 30초 후 steady state 5분 측정 |

## 측정 방법

각 시점에 대해 다음 항목을 모두 산출한다.

1. 절대 mCPU
   - `kubectl top pod -n ebpf-project -l app.kubernetes.io/name=gpuobs-agent`
2. 이벤트당 µs
   - `5분 동안 누적 CPU 시간` 을 같은 구간의 `kernel_launches_total + (h2d/d2h/dtod/unknown_dir 카운터의 sample 수)` 로 나눔
   - 카운터의 sample 수는 PromQL `sum(increase(gpuobs_cuda_kernel_launches_total[5m])) + sum(increase(gpuobs_cuda_h2d_bytes_total{...}[5m]) > bool 0) + ...` 로 산출
3. ringbuf drop 검사
   - `gpuobs_cuda_events_lost_total` 의 5분 increase 가 0 이어야 결과가 유효

## 결정 gate

baseline 대비 uprobe enabled 의 추가 CPU 사용이 **30 mCPU 미만** 이면 캐시 도입은 보류한다.
이유: dispatch hot path 비용이 충분히 작아 캐시 자료구조와 두 갈래 lookup 흐름이 도입하는
복잡도가 정량 효과를 정당화하지 못한다. 30 mCPU 이상의 추가 사용이 측정될 때만 후속 commit
시리즈 (podmap 자료구조, dispatch 통합, NVML 사이클 일괄 적재) 를 진행한다.

## 결과

### 시점 A: uprobe disabled (baseline)

`GPUOBS_CUDA_UPROBE_ENABLED=false` 로 동일 워크로드를 5분간 측정한 값이다.

| 항목 | 값 |
|---|---|
| 절대 mCPU | TBD |
| events_lost delta | n/a (uprobe 비활성) |
| 비고 | TBD |

### 시점 B: uprobe enabled (PR #39 머지 시점, v0.3.4)

본 문서 작성 시점의 main 브랜치 코드. 캐시 미도입 상태.

| 항목 | 값 |
|---|---|
| 절대 mCPU | TBD |
| 이벤트당 µs | TBD |
| events_lost delta | TBD (0 이어야 유효) |
| 비고 | TBD |

### 시점 C: cache enabled (본 PR 도입 후)

baseline 대비 30 mCPU 이상 차이가 측정될 때만 채워진다.

| 항목 | 값 |
|---|---|
| 절대 mCPU | TBD |
| 이벤트당 µs | TBD |
| events_lost delta | TBD |
| 비고 | TBD |

### 마이크로벤치 (`go test -bench`)

dispatch / buildActiveCudaKeys 의 ns/op 를 동일 머신에서 직접 측정한 값. 캐시 도입 전후의
hit / miss 경로 비교 기준이다.

| 벤치 | NoCache | CacheHit | CacheMiss |
|---|---|---|---|
| `BenchmarkReaderDispatch_*` | TBD | TBD | TBD |
| `BenchmarkReaderBuildActiveCudaKeys` | TBD | TBD | n/a |

## 결론

TBD (시점 B 측정 후 결정 gate 적용 결과를 명시, 시점 C 가 채워지면 baseline 대비 절대 감소량과
백분율로 50% 감소 수용 조건의 충족 여부를 판단한다).
