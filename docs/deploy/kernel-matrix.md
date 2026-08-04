# 기능별 최소 커널 매트릭스

본 문서는 netobs 관측 스택의 기능별 최소 커널 요구와 미충족 시의 degrade 동작을 한 표로 정리한다 (#297). 커널 하한은 기능마다 다르므로 단일 버전 표기 대신 본 매트릭스를 기준으로 삼는다. 전체 스택의 실질 하한은 event 수집이 의존하는 BPF ringbuf 의 5.8 이며, CUDA/NCCL uprobe 는 6.6 이 필요하다.

| 기능 | 최소 커널 | 근거 | 미충족 시 degrade |
|---|---|---|---|
| eBPF CO-RE 로드 (BTF) | `/sys/kernel/btf/vmlinux` 노출 (`CONFIG_DEBUG_INFO_BTF=y`, 배포판 커널은 5.4 부근부터 일반적) | kprobe 프로그램의 CO-RE relocation 이 커널 BTF 를 요구 | attach 실패가 `netobs_bpf_program_attach_retry_total{reason="btf_missing"}` 으로 분류되고 해당 프로그램 미적재 |
| event 수집 (BPF ringbuf) | 5.8 | `BPF_MAP_TYPE_RINGBUF` 도입 커널 | BPF object 로드 실패로 netobs 관측 자체가 비활성 (`netobs_bpf_program_loaded` 0, attach 실패 분류 메트릭) |
| kfree_skb drop reason 이름 해석 | 5.17 | `SKB_DROP_REASON` enum 과 tracepoint reason 필드 도입 | drop 수집은 동작하되 reason 이름이 `REASON_<code>` generic fallback 으로 표기 (tracefs format 파싱 실패 시 로그) |
| UDP 전용 pod 의 cgroup 귀속 (역매핑 스캐너, #228) | cgroup2 단일 계층 mount (버전보다 노드 cgroup 모드 의존) | 스캐너가 "cgroup id == 디렉터리 inode" 동일성 (cgroup2 전제) 에 의존 | v1/hybrid 노드는 시작 시 statfs 검증 (#297) 이 스캐너를 비활성하고 warn 로그와 `netobs_cgroup2_available` 0 으로 노출. TCP ringbuf 힌트 학습 경로는 유지 |
| CUDA/NCCL uprobe (gpuobs) | 6.6 | `uprobe_multi` BPF link (`BPF_TRACE_UPROBE_MULTI`) 전용. `perf_event_paranoid` 정책 차단을 피하려는 채택이라 구커널 폴백이 없음 | 심볼별 attach 실패 (`gpuobs_cuda_symbol_available` 0). 6.6 미만 노드는 `GPUOBS_CUDA_UPROBE_ENABLED=false` 권장, NVML 계열 device 메트릭은 계속 동작 |

참고 사항은 다음과 같다.

- BTF 는 커널 버전이 아니라 빌드 구성의 문제다. 미탑재 구형 커널을 위한 external BTF (BTFHub) 동봉은 다루지 않으며 `btf_missing` 분류로 식별한다
- kfree_skb reason 이름은 노드의 tracefs (`/sys/kernel/tracing/events/skb/kfree_skb/format`) 파싱으로 런타임 해석하므로 커널이 새 reason 을 추가해도 재빌드 없이 추종한다
- 온보딩 전제 조건과 절차는 [cluster-onboarding.md](cluster-onboarding.md) 를 따른다

## SYS_ADMIN 제거 판정 (#410)

netobs DaemonSet 의 base 는 `CAP_SYS_ADMIN` 을 포함한다. Ubuntu 커널 기본값인 `kernel.perf_event_paranoid=4` 가 `CAP_PERFMON` 까지 무력화해 `perf_kprobe` PMU 생성이 permission denied 로 실패하기 때문이다 (#216 canary 실측). paranoid 가 2 이하인 표준 커널에서는 `CAP_PERFMON` 만으로 attach 가 성립하므로 `deploy/netobs/overlays/no-sysadmin/patch-daemonset-no-sysadmin.yaml` 을 운영 overlay 의 `patches` 에 추가해 `CAP_SYS_ADMIN` 을 제거한다. `hostPID: true` 와 결합된 탈출 면적이 줄어드는 것이 이 제거의 목적이다.

판정은 다음 순서로 한다.

- 전 노드의 값 확인: `for n in $(kubectl get nodes -o name | cut -d/ -f2); do kubectl debug node/$n -it --image=busybox -- cat /host/proc/sys/kernel/perf_event_paranoid; done` 또는 각 노드에서 `sysctl kernel.perf_event_paranoid` 를 읽는다. DaemonSet 은 노드별 capability 분기가 없으므로 **한 노드라도 3 이상이면 적용하지 않는다**
- 적용 후 attach 전수 검증: `netobs_bpf_program_loaded` 가 전 심볼에서 1 인지, `netobs_bpf_program_attach_total` 이 증가하고 `netobs_bpf_program_attach_retry_total` 이 늘지 않는지 확인한다. 실패 시 `NetObsBpfProgramNotLoaded` 계열 alert 가 발화한다
- 실측 신호가 하나라도 어긋나면 즉시 patch 를 되돌린다. attach 실패는 관측 전면 중단이라 부분 degrade 가 아니다

현재 이 클러스터의 전 노드는 paranoid 4 (Ubuntu) 이므로 본 overlay 를 적용하지 않는다. overlay 는 표준 커널 클러스터로 확장할 때 쓰며, 렌더 결과는 `kubectl kustomize deploy/netobs/overlays/no-sysadmin` 으로 확인한다.

두 opt-out overlay 는 **검증된 구성이 아니라 준비된 구성**이다. 현 클러스터가 paranoid 4 라 실제 적용과 동작 검증이 불가능해 렌더링만 확인한 상태이므로, 처음 적용하는 클러스터에서는 위 attach 전수 검증을 생략하지 않는다. 특히 attach 실패는 부분 degrade 가 아니라 관측 전면 중단이라 canary 노드 없이 전 노드 동시 적용을 피한다.

## gpuobs SYS_PTRACE 제거 판정 (#410)

gpuobs 의 `CAP_SYS_PTRACE` 는 non-root GPU 워크로드의 `/proc/<pid>/environ` (mode 0400) 에서 `NVIDIA_VISIBLE_DEVICES` 를 읽어 multi-GPU 환경의 ordinal 과 UUID 매핑 정확도를 유지하는 데만 쓰인다. 단일 GPU 노드나 매핑 정확도가 불필요한 운영에서는 `deploy/gpuobs/overlays/no-ptrace/patch-daemonset-no-ptrace.yaml` 을 운영 overlay 의 `patches` 에 추가해 제거한다. `/proc/<pid>/cgroup` 기반 Pod 귀속은 cap 없이 동작하므로 pod 단위 GPU 메트릭은 유지된다.

- 적용 대상: GPU 가 노드당 1장이거나 `NVIDIA_VISIBLE_DEVICES=all` 로만 운영해 ordinal 매핑 모호성이 없는 클러스터
- 적용 후 확인: `gpuobs_pod_memory_used_bytes` 와 `gpuobs_pod_utilization_percent` 가 계속 emit 되는지, multi-GPU 노드에서 `gpu_uuid` 귀속이 어긋나지 않는지 확인한다
