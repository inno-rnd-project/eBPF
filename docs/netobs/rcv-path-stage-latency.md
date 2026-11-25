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

## `ack_wait`의 측정 범위

`ack_wait`는 수신측이 받은 데이터를 ACK하기까지 기다린 시간이다. `tcp_send_ack`(kprobe)에서 내며, 기준점은 `recv_starts`의 `ts_data`다.

- 시작: ACK를 받아야 하는 segment(payload나 FIN)가 `tcp_rcv_established`에 도착한 시각. 기준점이 비어 있거나 받은 데이터가 모두 ACK된 상태(`rcv_nxt == rcv_wup`)일 때 온 segment가 기준점을 잡으므로, 지연 ACK가 여러 segment를 묶어 ACK하면 대개 그중 첫 segment부터 잰다
- 끝: 그 데이터를 ACK하는 `tcp_send_ack` 호출. 새 데이터를 ACK하지 않는 호출(window update, dup ACK, keepalive 응답)은 값을 내지 않고 기준점만 비운다

payload 없는 순수 ACK는 기준점을 찍지 않는다. 찍으면 유휴 연결에서 상대의 다음 데이터가 올 때까지의 간격이 ACK 대기로 나간다.

한계는 다음과 같다.

- ACK가 송신 데이터에 piggyback되면 `tcp_send_ack`를 거치지 않아 표본이 되지 않는다
- `tcp_recvmsg` 진입이 `recv_starts` entry를 지운다. 데이터가 도착한 뒤에 시작한 읽기는 기준점을 지우므로 그 뒤에 나가는 ACK가 표본이 되지 않고, 읽은 뒤 ACK 전에 새 segment가 오면 그 segment부터 재서 값이 실제보다 짧게 나온다. 데이터보다 먼저 읽기에 들어가 기다리던 경우는 표본이 된다
- 커널이 `tcp_send_ack`를 인라인해 부르는 경로는 kprobe를 거치지 않아 표본이 되지 않는다. `tcp_send_delayed_ack`가 지연 ACK를 일찍 보내는 경우와, 6.8 노드에서 `tcp_backlog_ack_defer`가 켜져 `tcp_release_cb`가 ACK를 보내는 경우다. 그래서 커널 버전에 따라 분포가 다를 수 있다
- `tcp_rcv_established`를 거치지 않는 수신은 기준점을 찍지 않는다. FIN_WAIT1과 FIN_WAIT2에서 받은 FIN이나 데이터, handshake를 끝내는 segment에 실린 데이터가 그렇다. 그 ACK는 남아 있던 이전 기준점과 짝지어져 값이 실제보다 길게 나올 수 있다
- `recv_starts`의 키는 `skc_cookie`인데 cookie가 아직 할당되지 않은 소켓은 값이 0이라 한 entry를 함께 쓴다. 이런 소켓에서는 다른 소켓의 수신과 읽기가 기준점을 덮어쓰거나 지워 값이 실제와 다르게 나온다. `tcp_rcv_established`를 거친 수신이면 짧게만 나오고, 바로 위 항목의 경로에서는 다른 소켓의 기준점과 짝지어져 길게 나올 수 있다
- sock_diag(`ss` 등)는 조회한 소켓에 cookie를 할당하므로, 노드에서 실행하면 그 뒤로 해당 소켓의 측정 경로가 달라진다

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
