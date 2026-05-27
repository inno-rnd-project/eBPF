# drop event kernel stack trace capture

## 배경

#44 에서 `netobs_drop_events_labeled_total{drop_reason, drop_category}` 로 drop reason 과 카테고리가 노출되고 #64 에서 5-tuple flow context 가 추가되었지만 "원인이 발생한 지점" 의 진정한 식별은 kernel stack trace 까지 capture 해야 가능하다. 같은 reason 코드라도 호출 경로가 다르면 원인이 다르다 (예: `tcp_v4_do_rcv` → `tcp_filter` → drop vs `tcp_v4_rcv` → `__inet_lookup_skb` → drop).

본 PR 은 #83 의 4 stage (BPF stack id 수집, userspace symbol resolver, cardinality 가드, self-health 연계) 를 단일 series 로 도입한다.

## 4 stage 개요

| stage | 위치 | 책임 |
|---|---|---|
| BPF stack id 수집 | `bpf/netlat.bpf.c` 의 `handle_kfree_skb_reason` | `bpf_get_stackid` 로 `BPF_MAP_TYPE_STACK_TRACE` 에 stack 적재 후 id 만 ringbuf event 에 carry |
| userspace symbol resolver | `internal/netobs/symbols/` | `/proc/kallsyms` 파싱 과 KASLR offset 정규화 후 stack_id → (top_function, stack_hash) LRU cache |
| cardinality 가드 | `internal/netobs/metrics/dropstack.go` | namespace allow-list 와 top-N flow LRU 로 stack 메트릭 emit 제한 |
| self-health 연계 | `internal/netobs/selfhealth/refresher.go` | stack trace map utilization 과 resolver cache hit/miss counter 노출 |

## BPF struct 확장

`bpf/common.h` 의 `netobs_event` 에 `__s32 stack_id` 필드 와 `__u8 pad83[4]` 명시 패딩 슬롯을 추가한다. 본 슬롯 은 C 컴파일러 의 8-byte align trailing padding 에 의존하지 않고 #82 의 `pad82[6]` 와 동일 패턴 으로 layout 일관성 을 확보한다. struct size 는 96 → 104 byte 로 확장되며 Go 측 `Event` 의 `unsafe.Sizeof` 회귀 가드도 함께 갱신한다.

drop 외 stage (send 7 종, rcv 4 종) 의 emit 경로 는 `stack_id = -1` 로 명시 가드해 비-drop 메트릭 라벨 에 stack 차원이 새지 않게 한다.

## stack 수집 helper 선택

`bpf_get_stackid` + `BPF_F_FAST_STACK_CMP` flag 조합 으로 한정한다. `bpf_get_stack` 의 raw IP carry 는 ringbuf payload 폭 이 stack depth 에 비례 해 늘어나 본 PR 의 단일 정수 carry 의도와 어긋난다. dev cluster 의 kernel `6.2.0-33-generic` 에서 본 helper 셋 이 모두 stable 이다.

## userspace symbol resolver

`/proc/kallsyms` 는 DaemonSet 에 hostPath read-only 마운트 로 노출한다. `kptr_restrict=1` 환경 에서도 `privileged: true` 컨테이너 의 `CAP_SYSLOG` 로 실제 주소 reading 이 가능함 을 dev cluster 의 `netobs-agent` 에서 사전 검증했다.

resolver 의 핫 패스 비용 은 `stack_id → (top_function, stack_hash)` LRU cache (cap 1024) 로 회피한다. cache miss 시 `BPF_MAP_TYPE_STACK_TRACE` 의 `Lookup(stack_id)` 으로 IP 배열 을 얻고 사전 로드된 in-memory sorted symbol map 으로 O(log n) 검색해 frame 별 함수명 을 산정한다.

KASLR offset 은 `_text` 심볼 주소 기준 으로 정규화해 reboot 후 에도 동일 stack 이 동일 fingerprint 로 잡히게 한다. BPF program reload 시 stack_id 의미가 reset 되므로 `ebpfReady` false 전환 시 resolver cache 도 함께 invalidate 한다.

resolver 의 startup 이 실패해도 fail-open 정책 으로 stack 메트릭 만 skip 하고 기존 drop 메트릭 (`netobs_drop_events_labeled_total`, `netobs_drop_events_flow_total`) 은 정상 emit 한다.

### top_function 의 의미

`bpf_get_stackid` 의 stack 배열 은 stack[0] 이 가장 안쪽 frame 이라 `kfree_skb_reason` 자체가 항상 stack[0] 에 잡힌다. 그대로 노출 하면 모든 drop 이 동일 라벨로 묶여 변별력 이 사라지므로 다음 휴리스틱 으로 첫 caller frame 을 선택한다.

- stack[0] 부터 순회
- 함수명 이 `kfree_skb_reason`, `handle_kfree_skb_reason`, `kfree_skb` 중 하나면 skip
- 첫 번째 비-skip frame 을 `top_function` 으로 채택

### stack_hash 의 의미

`bpf_get_stackid` 가 반환하는 u32 stack id 를 hex 8 글자 로 표기 한다. stack 의 의미적 hash 가 아닌 단순 식별자 이며 BPF program reload 후 에는 동일 stack 이라도 다른 hash 로 잡힐 수 있다 (resolver cache invalidate 와 정합).

## cardinality 가드

기존 `DropFlowGuard` 와 동일 패턴 으로 `NetObsDropStackAllowNamespaces` 와 `NetObsDropStackMaxActive` env 를 도입한다. `dropEventsLabeled` 가 admit 한 flow 에 한해 stack 메트릭 을 추가 emit 해 라벨 폭주 를 회피한다. `DropFlowGuard` 와 admit 결과 가 독립이라 별도 max_active 로 cap 한다.

신규 메트릭 라벨 셋 은 다음 으로 고정한다.

```
netobs_drop_stack_total{node, src_namespace, src_workload, drop_reason, drop_category, stack_hash, top_function}
```

`top_function` 라벨 은 cap 64 의 first-N admit + startup grace period 의 sticky 정책 으로 폴딩한다. 첫 64 개 unique function 이 admit 되고 cap 도달 후 신규 function 은 `other` 로 묶인다. LRU 기반 의 시계열 flapping 위험 과 hash deterministic 의 불공정 분포 trade-off 를 회피한 MVP 결정 이며 본 정책 의 한계 (자주 등장하지 않는 신규 caller frame 이 startup 이후 에는 영구히 `other` 로 묶임) 는 follow-up 에서 top-by-count 정책 으로 대체 가능하다.

## self-health 연계

self-health refresher 의 `netobs_bpf_map_utilization_ratio` 에 `map="netobs_drop_stacks"` 라벨 을 추가해 stack map 포화 신호 를 노출한다. cilium/ebpf 의 `NextKey` 가 `BPF_MAP_TYPE_STACK_TRACE` 에서 정상 동작 하지 않을 경우 max_entries 만 노출 하고 entry count 는 0 으로 두는 fallback 을 채택한다.

resolver 효율 은 두 counter 로 분리 해 노출한다.

- `netobs_drop_stack_resolver_cache_hits_total`
- `netobs_drop_stack_resolver_cache_misses_total`

본 두 counter 의 비율 로 kallsyms 접근 실패 진단 과 cache cap 적정성 판단 이 가능 하다.

## capability 와 kernel 요건

DaemonSet 의 capability 는 본 PR 에서 추가 변경 하지 않는다. 현재 `privileged: true` 가 `CAP_BPF + CAP_PERFMON + CAP_SYSLOG` 를 모두 포함 하므로 stack trace map 의 `bpf_get_stackid` 호출 과 `/proc/kallsyms` 의 실제 주소 reading 이 양쪽 다 가능하다. 추후 capability selective add 의 follow-up 에서 세 cap 을 명시 추가해야 함을 본 docs 에 기록한다.

kernel 요건 은 `bpf_get_stackid` 의 stable kernel 4.6 이상, `BPF_MAP_TYPE_STACK_TRACE` 의 stable kernel 4.6 이상 이다. dev cluster 의 `6.2.0-33-generic` 은 양쪽 모두 충족한다.

## 비목표

- IPv6 drop 의 stack capture 는 본 PR 범위 밖 이다. 현재 `handle_kfree_skb_reason` 의 `NETOBS_AF_INET` 가드 와 정합 위해 follow-up 으로 분리한다
- userspace stack trace 는 본 PR 범위 밖 이다 (kernel stack 한정)
- DWARF inline expansion / source line resolution 은 별도 follow-up 이다
- dashboard 패널 / alert annotation / drill-down 연계 는 #87 / #88 / #89 / #90 의 follow-up 범위 다

## 회귀 검증

dev cluster 의 자연 drop 6.79/s (`NOT_SPECIFIED`, `QUEUE_PURGE`, `REASON_1`, `TC_EGRESS`, `QDISC_DROP`) 와 `observability-test` 의 cilium CNP DROP rule 을 활용해 reason 별 stack 분포 가 노출 되는지 확인 한다. CNP DROP 의 `kfree_skb_reason` 실제 호출 여부 는 `test/perf/drop-stack/verify.sh` 작업 시작 전에 `bpftrace` 로 사전 확인 하고 미호출 시 `nc` 비-listening 포트 로 `TCP_CLOSE` reason 을 유발 하는 fallback trigger 로 대체한다.
