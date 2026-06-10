# send path stage latency 분해

## 배경

#65 에서 receive path 의 stage 별 처리 시간이 `rcv_demux`, `rcv_established`, `rcv_app` 3 stage 로 분해되었지만 send path 는 `tcp_sendmsg` 단일 stage 의 latency 만 측정되었다. "단순히 느리다 가 아니라 커널 내부 처리에 NN ms 소요됨" 의 원래 목표를 send path 에서도 달성하기 위해 #82 에서 4 stage 로 분해한다.

## 4 stage 정의

| stage | kernel 함수 | 측정 의미 |
|---|---|---|
| `sendmsg_ret` | `tcp_sendmsg` (kretprobe) | syscall 진입부터 retn 까지의 전체 wall-clock |
| `tcp_write_xmit` | `tcp_write_xmit` (entry/ret pair) | TCP control path. cwnd / nagle / window throttle 의 비용 |
| `tcp_transmit_skb` | `__tcp_transmit_skb` (entry/ret pair) | 개별 segment transmit entry. TSO/GSO 활성 시 첫 segment 만 측정 |
| `to_devq` | `__dev_queue_xmit` (kprobe) | NIC queue 진입 직전 까지의 누적 latency |

## 6 stage 명세 와의 차이

이슈 #82 본문 의 6 stage 명세 (`syscall_sendmsg`, `tcp_sendmsg`, `tcp_write_xmit`, `ip_queue_xmit`, `dev_queue_xmit`, `driver tx`) 중 본 PR 에서 측정 가능한 자리는 4 stage 다. 다음 stage 는 본 PR 범위 밖이며 follow-up 으로 분리한다.

- `syscall_sendmsg`: `tcp_sendmsg` 가 syscall 진입 직후라 동등 함수로 대체 가능. 별도 kprobe 불필요
- `ip_queue_xmit`: kernel 6.x 에서 static inline 함수로 정의되어 kprobe attach 불가
- `ip_finish_output2`: 동일하게 static inline 으로 attach 불가
- `driver tx`: NIC vendor 종속 함수가 모두 static inline (i40e, mlx5, virtio 등). driver agnostic 한 최후단은 `__dev_queue_xmit` 이며 본 PR 에서 이미 측정 중이다

## TSO / GSO 활성 시 주의

대규모 sendmsg 호출은 TCP Segmentation Offload 또는 Generic Segmentation Offload 로 단일 `tcp_sendmsg` 가 N 회의 `__tcp_transmit_skb` 호출을 트리거한다. 본 PR 의 `seen_transmit` flag 가드는 starts map 의 slot race 회피 목적으로 첫 segment 만 `tcp_transmit_skb` stage 의 latency 를 측정하므로 `netobs_pod_stage_latency_labeled_seconds{stage="tcp_transmit_skb"}` 의 값은 per-syscall 의 첫 segment latency 다. 전체 segment 의 합산 latency 는 #121 의 `netobs_send_path_full_latency_seconds` histogram 으로 별도 emit 된다.

## Segment 누적 latency (#121)

#82 의 첫 segment 한정 측정의 보완으로 #121 에서 socket_cookie 기반 segment 누적 BPF map (`seg_accum`) 을 도입했다. 본 절은 신규 흐름의 BPF 동작과 메트릭 의미와 운영자 활용 시나리오를 정리한다.

### BPF 동작

`bpf/netlat.bpf.c` 의 `seg_accum` map 은 `BPF_MAP_TYPE_LRU_HASH` 타입에 max_entries 8192 로 cardinality cap 한다. key 는 `socket_cookie` (u64), value 는 첫 transmit timestamp 와 누적 latency (nanoseconds) 와 segment 개수 를 보관하는 struct 다.

- `handle_tcp_transmit_skb` 의 모든 segment entry 마다 `ts_segment_entry` 를 갱신하고 `seg_accum` 의 segment_count 를 증가
- `handle_tcp_transmit_skb_ret` 의 모든 segment ret 마다 segment 단위 latency (now - ts_segment_entry) 를 `seg_accum.cumulative_latency_ns` 에 누적
- `handle_tcp_sendmsg_ret` 시점에 `seg_accum` 조회 후 누적 결과를 ringbuf event 의 `full_latency_ns` 와 `segment_count` 필드에 carry 한 뒤 map entry 를 cleanup

기존 첫 segment 의 stage_latency emit 흐름은 `seen_transmit` flag 로 그대로 유지되어 stage_latency 메트릭의 회귀 zero 다.

### 신규 메트릭 2종

- `netobs_send_path_full_latency_seconds` histogram 은 sendmsg 사이클 의 모든 `tcp_transmit_skb` segment latency 합산 (seconds) 을 emit. bucket 분포는 stage_latency 와 동일한 1µs 부터 시작하는 지수 분포 20단계. 라벨 셋은 `node` 와 `src_namespace` 와 `src_pod` 와 `src_pod_uid` 와 `traffic_scope` 와 `direction` 과 `dst_namespace` 와 `dst_workload` 와 `dst_pod_uid` 의 9종
- `netobs_send_path_segment_count_total` counter 는 segment 개수 누적 합산. `Add(segment_count)` 형식으로 segment_count 자체를 라벨로 두지 않고 누적해 cardinality 폭증 회피

### 운영자 활용 시나리오

- TSO/GSO 활성 환경의 large message latency 정확 추적. `netobs_pod_stage_latency_labeled_seconds{stage="tcp_transmit_skb"}` 의 p99 가 낮아 보여도 `netobs_send_path_full_latency_seconds` 의 p99 가 높으면 segment 합산 비용이 커널 내부에서 누적되고 있다는 신호
- segment 분할 빈도 모니터링. `rate(netobs_send_path_segment_count_total[5m])` 와 `rate(netobs_pod_stage_events_labeled_total{stage="sendmsg_ret"}[5m])` 의 비율로 평균 segment 개수 산출 가능. 비율이 큰 Pod 는 large message 발생기 식별 후보

### 한계와 follow-up

- segment 개별 latency 분포 는 emit 하지 않는다. cardinality 폭증 회피 위해 합산만 노출하며 분포 추적이 필요하면 별도 PR 에서 percentile bucket 별 sample 추출 검토
- MIG 와 GRO 와 LRO 같은 receive path 의 segment 합산은 본 범위 밖이며 별도 이슈로 분리
- `seg_accum` map 의 `max_entries` 는 정적 8192 고정. 다수 동시 socket 운영 환경에서 LRU eviction 으로 일부 sendmsg 의 누적이 유실 될 수 있고 운영 환경별 동적 조정은 별도 PR

## 본질 라벨 셋

`netobs_stage_latency_labeled_seconds_bucket` 메트릭은 #65 와 동일 라벨 셋 (`stage`, `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `dst_namespace`, `dst_workload`) 을 유지한다. 신규 2 stage 는 `direction=egress` 로 emit되며 dashboard 의 PromQL 그룹화 (#76 표준) 가 추가 코드 없이 자동 적용된다.

## dashboard 참조

`deploy/dashboards/netobs.json` 의 panel 1 (Send path stage latency p99) 가 4 stage 의 stage 별 p99 를 timeseries 로 노출한다. panel 2 (Receive path stage latency p99) 와 한 화면 좌우 배치로 양방향 stage breakdown 을 즉시 비교 가능하다.
