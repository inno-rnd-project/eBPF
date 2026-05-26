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

대규모 sendmsg 호출은 TCP Segmentation Offload 또는 Generic Segmentation Offload 로 단일 `tcp_sendmsg` 가 N 회의 `__tcp_transmit_skb` 호출을 트리거한다. 본 PR 의 `seen_transmit` flag 가드는 starts map 의 slot race 회피 목적으로 첫 segment 만 latency 를 측정하므로 노출된 값은 **per-syscall 의 첫 segment latency** 다. 전체 segment 의 합산 latency 가 필요하면 follow-up 으로 skb pointer 키 기반 별도 map 도입이 필요하다.

## 본질 라벨 셋

`netobs_stage_latency_labeled_seconds_bucket` 메트릭은 #65 와 동일 라벨 셋 (`stage`, `node`, `src_namespace`, `src_workload`, `traffic_scope`, `direction`, `dst_namespace`, `dst_workload`) 을 유지한다. 신규 2 stage 는 `direction=egress` 로 emit되며 dashboard 의 PromQL 그룹화 (#76 표준) 가 추가 코드 없이 자동 적용된다.

## dashboard 참조

`deploy/dashboards/netobs.json` 의 panel 1 (Send path stage latency p99) 가 4 stage 의 stage 별 p99 를 timeseries 로 노출한다. panel 2 (Receive path stage latency p99) 와 한 화면 좌우 배치로 양방향 stage breakdown 을 즉시 비교 가능하다.
