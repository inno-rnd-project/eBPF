# receive path stage latency 측정

## 배경

#65에서 receive path가 `rcv_demux`와 `rcv_established`와 `rcv_app` 3 stage로 분해되고 Pod 귀속과 TCP 상태 sample 집계까지 도입됐지만, stage별 latency는 측정되지 않고 `emit_rcv_event`가 `latency_us=0`으로 고정 emit했다. 그 결과 송신 경로는 stage별 커널 처리 시간을 노출하는데 수신 경로는 dashboard 패널이 빈 데이터로 남았다. #141은 송신 경로의 누적 차분 패턴을 수신 경로에 적용해 "단순히 느리다가 아니라 커널 내부 처리에 NN ms 소요됨"을 수신 방향에서도 달성한다.

## 측정 방식

송신 경로가 `tid` 키로 `starts` 맵을 쓰는 것과 달리 수신 경로는 softirq context라 `tid`가 무의미하다. 따라서 `socket_cookie` 키 LRU 맵 `recv_starts`로 동일 connection의 stage 진입 시점을 묶는다. `recv_starts`의 value는 L3 진입 시각 `ts_l3`와 established 처리 시각 `ts_established` 두 timestamp를 보관한다.

| stage | kernel 함수 | 측정 의미 | 기준점 |
|---|---|---|---|
| (stash) | `tcp_v4_rcv` / `tcp_v6_rcv` | L3 진입 시각 `ts_l3` 기록 (event emit 없음) | - |
| `rcv_demux` | `tcp_v4_do_rcv` / `tcp_v6_do_rcv` | early demux 성공 시 L3 진입부터 demux까지 (1ms 이내), 아니면 0 | `ts_l3` (recent) |
| `rcv_established` | `tcp_rcv_established` | do_rcv부터 established 처리까지의 커널 RX 처리 시간 | do_rcv 진입 시각 |
| `rcv_app` | `tcp_recvmsg` | established 이후 app이 데이터를 읽기까지의 pickup 대기 | `ts_established` |

`rcv_demux`는 `tcp_v4_do_rcv` 진입 시점에 `ts_l3`를 now로 재설정해 `rcv_established`의 기준점을 매번 갱신한다. early demux로 `tcp_v4_rcv`가 직전에 stash한 경우만 L3 진입부터 demux까지 (`NETOBS_RCV_DEMUX_MAX_NS` 1ms 이내) 를 `rcv_demux` latency로 측정하고, 그 외에는 0이다. `rcv_established`는 do_rcv부터 established까지의 커널 RX 처리 시간을 산정하고 `ts_established`를 갱신하며, `rcv_app`이 그 기준으로 app pickup 대기를 산정한 뒤 `recv_starts` entry를 cleanup한다. `stash_recv_l3`와 `rcv_demux`는 `ts_l3`만 갱신하고 `ts_established`를 보존해, 고트래픽 환경에서 app read 이전 도착한 다음 패킷이 app pickup 측정 기준을 0으로 날리지 않게 한다.

## tcp_v4_rcv의 sk 복원

`tcp_v4_rcv`는 socket lookup 이전 단계라 sock 인자가 없다. #141은 `skb->sk` (early demux 결과) 로 sock을 복원해 `socket_cookie`를 얻는다. established 연결은 early demux로 `skb->sk`가 채워져 있어 `ts_l3` stash가 가능하고, 신규 SYN이나 listen socket처럼 `skb->sk`가 null인 케이스는 stash를 skip한다. `ts_l3`가 부재하면 `rcv_demux`와 `rcv_established`가 `latency_us=0`으로 자연 fallback한다.

## Pod 귀속

수신 Pod 귀속은 #65에서 이미 도입된 `sock_cgroup_id()`로 수행한다. softirq context에서 `bpf_get_current_cgroup_id()`가 인터럽트당한 task의 cgroup을 가리키는 문제를 `sk_cgrp_data` 기반 `sock_cgroup_id()`로 우회해 수신 Pod의 cgroup_id를 복원한다. `emit_rcv_event`가 5-tuple의 src와 dst를 swap해 ingress event의 src를 remote peer로, dst를 local Pod로 채우므로 라벨 의미가 송신 경로와 일관된다.

## 메트릭과 dashboard

수신 stage latency는 송신 경로와 동일한 `netobs_stage_latency_labeled_seconds` histogram에 Observe되며 `stage` 라벨 (`rcv_demux`와 `rcv_established`와 `rcv_app`) 로 송신과 구분된다. `deploy/dashboards/netobs.json`의 Receive path stage latency 패널이 본 rcv stage 시리즈를 쿼리하므로 별도 패널 변경 없이 실데이터가 표시된다. TCP 상태 gauge (`netobs_rcv_tcp_*`) 는 #65의 `rcv-path-tcp-state.md` 흐름으로 그대로 유지된다.

## 한계와 follow-up

- `rcv_demux`는 early demux로 `skb->sk`가 채워진 established 연결만 L3 진입 기준 latency를 갖고, cross-node forwarding처럼 early demux가 없는 경로에서는 0이다. 이 경우에도 `rcv_established`는 do_rcv 기준으로 정상 측정된다.
- `rcv_app`은 "커널 내부 처리 시간"이 아니라 socket 수신 큐 대기와 app 스케줄 지연을 합친 app pickup 대기다. 한 socket에 여러 패킷이 도착하면 마지막 established 시각 기준이라 근사값이다.
- `socket_cookie`는 socket 종료 후 재할당될 수 있어, `rcv_app`이 cleanup하기 전 socket이 닫히면 stale entry가 남는다. `rcv_app`의 `ts_established` 차분이 `NETOBS_RCV_STALE_NS` (10s) 를 넘으면 stale로 간주해 latency를 채택하지 않고, `rcv_demux`가 다음 패킷에서 기준점을 재설정한다.
- pod 단위 수신 latency (`netobs_pod_stage_latency_labeled_seconds`) 는 송신 Pod (Src) 라벨 기반이라 ingress event에는 적용되지 않는다. 수신 Pod (Dst) 단위 latency 노출은 별도 follow-up으로 분리한다.
- `recv_starts` 맵의 `max_entries`는 정적 8192 고정이며 다수 동시 socket 환경에서 LRU eviction으로 일부 stage의 기준점이 유실될 수 있다.
