# BPF map race safety audit 결과 (#107)

본 문서는 `bpf/netlat.bpf.c` 와 `bpf/common.h` 에 정의된 BPF map 7종과 `internal/netobs/` 의 userspace map 핸들 접근 패턴 4종 에 대한 race condition audit 결과를 정리한다. 이슈 #107 본문이 명시한 `flow_bytes` 와 `pod_bytes` 2종 외에 BPF map / userspace map 의 race 가능성이 audit 미실시 였던 영역을 모두 cover 한다.

## audit 종합 결론

netobs 측 BPF map 7종과 userspace 핸들 패턴 4종 모두 race-free 검증 완료. 신규 발견된 race 없음. 본 PR 이전 정정 4건 (PR #82 `21ec1e9`, PR #85 `2bb0dd5`, PR #83 `9b59fcd`, PR #87 `3fe71d0`) 의 정합성 재확인. `target_daddr` ARRAY 의 TOCTOU 평가는 운영 영향 zero 로 결론 (test 환경 한정의 deny-all/match-one 패턴 이라 transient race 발생 시 모두 통과로 fallback 안전).

## BPF map 7종 audit 표

| Map | Type | max_entries | update 패턴 | race 보호 | 결론 |
|---|---|---|---|---|---|
| `starts` | LRU_HASH | 16384 | `bpf_map_update_elem(BPF_ANY)` (tid 기반 entry-delete) | kernel LRU spin lock + #82 socket_cookie 가드 (PR `21ec1e9`) | race-free |
| `flow_bytes` | LRU_HASH | 1024 | lookup → `BPF_NOEXIST` zero init → re-lookup → `__sync_fetch_and_add` | atomic add (#85 PR `2bb0dd5`) | race-free |
| `pod_bytes` | LRU_PERCPU_HASH | 16384 | lookup → simple assign | per-CPU 독립 슬롯 | race-free (per-CPU 보장) |
| `events` | RINGBUF | 16 MiB | `bpf_ringbuf_reserve` → `bpf_ringbuf_submit` pair | kernel ringbuf lock-free | race-free |
| `events_dropped` | PERCPU_ARRAY | 1 | non-atomic increment (`(*v)++`) | per-CPU 독립 | race-free (per-CPU 보장) |
| `drop_stacks` | STACK_TRACE | 10240 | `bpf_get_stackid(BPF_F_FAST_STACK_CMP)` | idempotent lookup-or-insert | race-free |
| `target_daddr` | ARRAY | 1 | userspace write + BPF read | TOCTOU 가능 (test 환경 한정) | 운영 영향 zero (아래 별도 절) |

### `target_daddr` TOCTOU 평가

`target_daddr` 는 ARRAY map 으로 userspace 가 startup 시점에 1회 write 하고 BPF kprobe 가 매 sendmsg / retransmit_skb 호출 마다 read 하는 구조다. write 와 read 사이의 timing 보장이 없어 transient race 가능성이 이론적으로 존재 하지만 다음 3가지 사실로 운영 영향이 zero 다.

- `match_target` 의 분기 가 `target == 0` 또는 `target == NULL` 면 1 (= 모두 통과) 을 반환 하는 fallback-allow 패턴 이라 race 가 발생 해도 false positive (의도치 않은 통과) 만 발생 하며 false negative (정상 트래픽 차단) 는 불가
- userspace write 는 agent startup 시점 1회 만 발생 하고 runtime 변경 경로가 없어 race window 가 단일 부팅 사이클 의 sub-second 범위에 한정
- IPv6 path (`tcp_v6_rcv` / `tcp_v6_do_rcv` / `udpv6_*`) 는 본 필터 미적용으로 항상 통과 (IPv4 only filter) 이며 본 race 와 무관

따라서 정정 불필요. 다만 `bpf_atomic_xchg` 도입은 향후 IPv6 filter 도입 (#103 follow-up) 시 재검토 가능.

## userspace 핸들 패턴 4종 audit 표

| 패턴 | 위치 | 용도 | race 보호 | 결론 |
|---|---|---|---|---|
| `atomic.Pointer[cebpf.Map]` Store-once | `flow/collector.go:61`, `podbytes/collector.go:69` | BPF map 핸들 startup 주입 | Store 1회 + Load N회 (lock-free) | race-free |
| `sync.RWMutex` RLock-to-Lock promote | `metadata/enricher.go:32` | flow swap + ResolvePID idempotent 가드 | RUnlock 후 Lock 재획득 사이 TOCTOU 를 `if _, already := ...; already` idempotent 분기로 흡수 | race-free |
| `atomic.Value` resolver 핸들 | `metrics/dropstack.go:31` | drop stack resolver onReady startup Store + emit Load | Store 1회 + Load 다회 (zero value safe) | race-free |
| `sync.Mutex` 단순 보호 | `metrics/{flowguard,tcpstate,dropflow,dropstack}.go` | 단일 cycle 안 emit/evict 직렬화 | mutex 보호 | race-free |

### `metadata/enricher.go` promote 경로 상세

`Enricher.ResolvePID` 의 promote 경로 (`metadata/enricher.go:85-115`) 가 다음 invariant 를 유지한다.

- `RLock()` 으로 flowCurrent / flowPrevious 조회
- cache miss 시 `RUnlock()` 후 lookup
- `Lock()` 재획득 후 다른 goroutine 이 이미 promote 했는지 `if _, already := e.flowCurrent[cookie]; !already` 로 확인
- 이미 promote 되었으면 자기 lookup 결과 폐기, 아니면 자기 결과 적재
- 본 패턴은 RUnlock 과 Lock 사이의 TOCTOU 를 idempotent 가드로 회피해 race 결함 없음

본 PR 의 단위 테스트 reproducer (`TestEnricher_RWMutexPromoteIdempotent`) 가 다중 goroutine 동시 ResolvePID + flow swap 시 본 invariant 유지를 영구 회귀 가드로 검증한다.

## PR 별 race 정정 히스토리

본 audit 이전에 race-prone 패턴이 PR 4건 으로 정정된 흔적을 git log 로 확인했다. 본 audit 은 본 정정들의 현재 정합성을 재검증한 결과이며 모든 정정이 유지되고 있다.

- PR #82 `21ec1e9` send path: socket_cookie 가드 도입으로 `tcp_write_xmit` 와 `tcp_transmit_skb` 사이의 cross-socket race 차단. `starts` map 의 entry 식별자 가 tid 단독 에서 (tid, socket_cookie) 페어 로 강화
- PR #85 `2bb0dd5` flow 추적: `inc_flow_bytes` 의 lookup-miss-update race 를 `BPF_NOEXIST` zero init + 재-lookup + `__sync_fetch_and_add` 의 3 단계 패턴 으로 정정. `BPF_ANY` 단발 update 시 동시 lookup miss 한 두 CPU 의 init 가 서로 덮어 써 delta 가 유실 되던 결함 해소
- PR #83 `9b59fcd` drop stack: drop stack resolver 핸들 을 `atomic.Value` 로 전환 해 onReady 1회 Store + emit 다회 Load 흐름 의 zero value safe 보장
- PR #87 `3fe71d0` Pod 메트릭 토글: `podMetricsEnabled` 와 `dstClassifier` 를 `atomic.Bool` / `atomic.Pointer` 로 전환 해 init race-free

## race detector 가 잡지 못하는 영역의 운영적 식별 절차

Go race detector (`-race`) 는 userspace 의 동일 goroutine 간 race 만 잡고 BPF ↔ userspace 경계의 race 는 감지 불가. 본 audit 의 e2e 가드 `test/perf/bpf-map-race/verify.sh` 가 다음 3 신호로 BPF 측 race 를 운영적으로 식별한다.

- counter monotonic 위반 (`netobs_flow_bytes_total` 또는 `netobs_pod_bytes_total` 의 일시적 감소): lookup-miss-update race 의 delta 유실 또는 stale read 의 signature
- BPF map utilization 의 entries 누락 (`netobs_bpf_map_utilization_ratio{map="starts"}` 가 active connection 수 대비 부족): LRU evict timing 의존 race 의 signature
- BPF ringbuf 수신 count 와 BPF 측 emit count 사이의 divergence 가 `netobs_bpf_ringbuf_drops_total` 로 설명 불가: ringbuf reserve/submit pair 의 atomicity 결함 signature

## 검증 가드

본 audit 의 영구 회귀 가드는 다음 2 단계로 구성된다.

- 단위 테스트: `go test -race -count=10 ./internal/netobs/...` 10 회 반복 안정 통과. 본 PR 의 단위 테스트 reproducer (`TestCollector_ConcurrentMultiCPURace` 의 flow / podbytes 변종 과 `TestEnricher_RWMutexPromoteIdempotent`) 가 multi-CPU 동시 호출 시나리오 의 race 표면적을 영구 확보
- e2e 가드: `test/perf/bpf-map-race/verify.sh` 의 3 시나리오 (multi-stream 트래픽 counter monotonic / rapid Pod lifecycle BPF map utilization / drop event divergence). 비결정적 timing 의존 이라 1차 fail-on-miss 와 2-3차 warn-only 로 분리

## 비목표 (별도 이슈 위임)

- `internal/gpuobs/cuda/` 의 BPF map race (`cuda_tid_device` / `cuctx_to_device` / `cuctx_create_args` / `sync_starts` 와 userspace `podMap` / `visDev` 의 stale read 가능성) 는 본 audit 비목표. gpuobs / cuda 도메인 분리 정책 유지
- gpuobs-agent 의 NVML 호출 race 는 이슈 #107 본문 비목표 그대로 별도 이슈 위임
- BPF map 의 sizing / LRU eviction 정확성 audit 은 본 이슈 외 (별도 sizing 검증 이슈)
- BPF program 자체의 runtime crash 추적은 본 audit 외 (#105 의 attach self-health 와 별개 도메인)
