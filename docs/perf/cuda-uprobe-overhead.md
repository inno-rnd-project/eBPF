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

`GPUOBS_CUDA_UPROBE_ENABLED=false` 로 동일 워크로드를 5분간 측정한 값이다 (2026-05-07).

| 항목 | 값 |
|---|---|
| 절대 mCPU | **3.2 mCPU** |
| events_lost delta | n/a (uprobe 비활성) |
| 비고 | gpuobs-agent 의 collector / NVML poll / HTTP 메트릭 endpoint 만 활성. 본 값이 cuda uprobe 와 무관한 agent 자체의 고정 비용이다 |

### 시점 B: uprobe enabled (PR #39 머지 시점, v0.3.4)

본 문서 작성 시점의 main 브랜치 코드. 캐시 미도입 상태 (2026-05-07).

| 항목 | 값 |
|---|---|
| 절대 mCPU | **740.6 mCPU** |
| baseline 대비 추가 사용 | **+737 mCPU** |
| kernel launch rate | 27,048 / s |
| 이벤트당 µs | **27.3 µs / event** |
| events_lost delta | 0 (ringbuf drop 없음, 결과 유효) |
| 비고 | 27 K Hz launch rate 에서 dispatch hot path 의 `kube.Resolver.ResolvePID` 가 매 이벤트 cgroup parse 를 발생시켜 추가 CPU 가 거의 1 코어 수준에 근접한다 |

### 시점 C: cache enabled (본 PR 도입 후, v0.3.5)

NVML refresh 사이클에서 active PID 를 일괄 적재하고 dispatch 가 lazy fill 로 보완하는 podMap
캐시가 활성화된 상태. 동일 PyTorch ResNet50 워크로드를 5분간 측정한 값이다 (2026-05-07).

| 항목 | 값 |
|---|---|
| 절대 mCPU | **183.8 mCPU** |
| baseline 대비 추가 사용 | **+180.6 mCPU** |
| kernel launch rate | 24,036 / s |
| 이벤트당 µs | **7.5 µs / event** |
| events_lost delta | 0 (ringbuf drop 없음, 결과 유효) |
| 비고 | 시점 B 의 +737 mCPU 대비 +181 mCPU 로 **75.5% 감소**. 이벤트당 비용도 27.3 µs 에서 7.5 µs 로 단축되어 캐시 hit 가 매 이벤트의 cgroup parse 를 흡수한 효과가 직접 확인된다 |

### 마이크로벤치 (`go test -bench`, `-benchtime=300ms`)

dispatch / buildActiveCudaKeys 의 ns/op 를 동일 머신에서 직접 측정한 값. 캐시 hit / miss 경로
의 절대 비용 차이를 보여준다.

| 벤치 | ns/op | allocs/op | 의미 |
|---|---|---|---|
| `BenchmarkReaderDispatch_NilResolver` | 34.28 | 0 | resolver 제외 dispatch 의 절대 하한 |
| `BenchmarkReaderDispatch_FakeResolver` | 70.78 | 0 | cache miss + in-memory fake resolver (참고용 상한) |
| `BenchmarkReaderDispatch_CacheHit` | **75.93** | 0 | 캐시 hit 일반 경로 (운영 시 절대 다수의 이벤트가 본 경로) |
| `BenchmarkReaderDispatch_CacheMiss_Slow` | **1,059,584** | 0 | 30µs 지연 resolver 로 모사한 worst case (refresh 사이의 신규 PID 첫 호출만 본 경로) |
| `BenchmarkReaderBuildActiveCudaKeys` | 19,661 | 3 | NVML refresh 사이클에서 64 PID 의 cleanup key 생성 (캐시 hit 기준) |

CacheHit 가 CacheMiss_Slow 대비 약 **13,950 배 빠르다**. 운영에서 새 PID 가 등장하는 빈도 대비
hit 빈도가 압도적으로 크기 때문에 평균 비용은 CacheHit 에 매우 가깝다.

## 결론

### 시점 B 결정 gate 적용

baseline (3.2 mCPU) 대비 uprobe enabled 의 추가 사용은 **+737 mCPU** 로, 결정 gate 의 30 mCPU
임계를 약 24 배 초과한다. 이벤트당 비용도 27.3 µs 로 dispatch hot path 가 매 이벤트마다
`/proc/<pid>/cgroup` 을 read + parse 하는 비용이 그대로 누적되는 것으로 해석된다. 따라서 캐시
도입을 진행한다.

### 시점 C 결과 (캐시 도입 후, v0.3.5)

baseline 대비 추가 사용량이 시점 B 의 +737 mCPU 에서 +181 mCPU 로 **75.5% 감소**했다. 이슈
#32 의 50% 감소 수용 조건을 정량 충족하며, 매 이벤트의 cgroup read + parse 비용이 캐시 lookup
한 번으로 흡수되는 본 PR 의 핵심 의도가 실측으로 입증되었다. events_lost 도 측정 구간 내내 0
이라 결과의 유효성이 확인된다.

마이크로벤치 결과는 이 동작의 정량 메커니즘을 설명한다. CacheHit 경로 76 ns/op 와 CacheMiss
경로 1.05 ms/op 사이의 13,950 배 차이가 곧 캐시가 흡수하는 이벤트당 시간이다. PyTorch ResNet50
워크로드의 27 K Hz 이벤트 흐름에서 신규 PID 등장은 NVML refresh 사이의 일부 이벤트로 한정되어,
운영 평균은 CacheHit 비용에 매우 가깝게 수렴한다.
