# cuda uprobe 측정 워크로드

본 디렉토리는 `gpuobs-agent` 의 cuda uprobe 모듈이 dispatch hot path 에서 발생시키는 CPU
overhead 를 정량 측정하기 위한 표준 워크로드 매니페스트를 모은다. 본 매니페스트들은 측정
재현성을 보장하기 위해 dev / prod 양쪽에서 동일하게 사용 가능한 형태로 작성되어 있다.

측정 보고서 본문은 [docs/perf/cuda-uprobe-overhead.md](../../docs/perf/cuda-uprobe-overhead.md)
에 분리해 둔다.

## 매니페스트

| 파일 | 워크로드 | 자극 경로 |
|---|---|---|
| `vectoradd-loop.yaml` | cuda-sample vectorAdd 무한 반복 | cuLaunchKernel + cuMemcpyHtoD_v2 + cuMemcpyDtoH_v2 |
| `pytorch-resnet50-bench.yaml` | PyTorch ResNet50 inference 무한 루프 | 전 심볼군 (kernel launch 수천 Hz, h2d / d2h 다수) |
| `pytorch-conv2d-bench.yaml` | Conv2d 만 반복 호출 (cuDNN benchmark on) | cuMemcpy2D 경로의 BPF 4-6회 bpf_probe_read_user 자극 |
| `pytorch-cuda-stream-sync-bench.yaml` | 명시적 stream / event 로 매 iteration 마다 동기화 3종 호출 | cuStreamSynchronize 2회 + cuEventSynchronize 1회 + cuStreamWaitEvent 1회 (#67 검증용) |

## 실행 절차

1. `kubectl apply -f test/perf/<manifest>.yaml`
2. Pod 가 `Running` 상태로 들어간 뒤 최소 5분 대기 (warmup + NVML poll 안정화)
3. 측정 명령:
   - `kubectl top pod -n ebpf-project gpuobs-agent-<id>` 로 절대 mCPU 측정
   - `gpuobs_cuda_kernel_launches_total` 의 `rate(...[1m])` 으로 launch rate 산출
   - `gpuobs_cuda_events_lost_total` 의 delta 가 0 인지 확인 (0 이 아니면 측정 무효)
4. 매니페스트 정리: `kubectl delete -f test/perf/<manifest>.yaml`

## 주의사항

- pytorch 이미지는 약 6.5 GB 이며 처음 pull 시 dev 노드의 ephemeral storage 가 일시적으로
  20% 이상 점유된다. DiskPressure 임계 근처라면 pull 전에 `docker image prune -af` 로
  공간을 확보한다.
- 모든 워크로드는 단일 GPU 만 사용한다. multi-GPU NCCL allreduce 같은 D2D 경로 측정은 dev
  노드가 단일 GPU 라 본 디렉토리의 매니페스트로는 다루지 않는다.
- 측정 중에는 동일 노드에 다른 cuda 컨테이너를 동시 기동하지 않는다 (라벨 카디널리티가
  불필요하게 늘어나 PromQL 집계가 어려워진다).

## 워크로드 lifecycle (#319)

본 디렉토리의 워크로드는 전부 일회성 검증용이며 클러스터 상주를 금지한다. 과거 correlation-stress 와 observability-test 워크로드가 검증 후 정리되지 않고 수십 일 상주하다 ephemeral-storage 고갈 시점에 Evicted 잔재를 남긴 전례가 있다.

- 검증이 끝나면 즉시 `kubectl delete -f <매니페스트>` 로 정리한다. Job 은 완료돼도 pod 잔재가 kube-state-metrics 집계 (overview 의 terminated) 에 남으므로 Job 리소스째 삭제한다
- 별도 네임스페이스에 워크로드를 임시 생성한 경우 (correlation-stress 류) 워크로드와 Service 까지 함께 삭제한다
- 매니페스트에는 ephemeral-storage requests/limits (공통 100Mi/1Gi) 를 명시한다. 새 매니페스트 추가 시 동일 규약을 따른다
