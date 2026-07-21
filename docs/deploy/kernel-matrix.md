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
