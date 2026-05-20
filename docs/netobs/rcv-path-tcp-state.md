# receive path latency 와 TCP 상태 운영 가이드

`netobs_tcp_state_*` 3 종 gauge 와 `NetObsTcpCongestion` alert 로 수신 Pod 의 TCP 혼잡 신호를 진단하는 운영 워크플로다. 본 도구는 #65 에서 도입되었으며 기존 send path 위주의 stage latency 관측을 보완해 receive path 의 stage 별 도착 시점과 connection 단위 혼잡 상태를 동시에 노출한다.

## 메트릭 카탈로그

- `netobs_tcp_state_min_cwnd{namespace, pod, node}` gauge. scrape window 안 sample 의 최소 snd_cwnd (segment 단위)
- `netobs_tcp_state_max_srtt_seconds{namespace, pod, node}` gauge. scrape window 안 sample 의 최대 srtt (초 단위, kernel `<<3` scale 제거됨)
- `netobs_tcp_state_min_ssthresh{namespace, pod, node}` gauge. scrape window 안 sample 의 최소 snd_ssthresh. `TCP_INFINITE_SSTHRESH` sentinel 은 집계에서 제외

3 종 gauge 는 scrape 시점에 누적치가 reset 되며 다음 window 가 직전 결과를 끌고 오지 않는다. sample 이 없는 Pod 는 시리즈 자체가 emit 되지 않아 stale 시계열로 남지 않는다.

## stage 분류

receive path 의 BPF kprobe 는 4 종 stage 로 분해된다.

- `rcv_l3` `tcp_v4_rcv`. L3 entry 로 sock lookup 이전이라 수신 Pod 귀속이 불가하고 본 PR 에서는 emit 을 보류 (attach 만 유지)
- `rcv_demux` `tcp_v4_do_rcv`. socket lookup 직후 첫 stage 로 sock 인자가 있어 cgroup 식별 가능
- `rcv_established` `tcp_rcv_established`. established 소켓 packet 처리
- `rcv_app` `tcp_recvmsg`. userspace recv syscall 진입 (process context)

`bpf_get_current_cgroup_id` 가 softirq context (rcv_demux / rcv_established) 에서 swapper 의 cgroup 을 반환하는 한계는 `sock->sk_cgrp_data` 기반 helper 로 우회한다.

## NetObsTcpCongestion alert

수신 Pod 의 RTT 와 cwnd 가 동시에 악화될 때만 발화하는 합성 alert 다.

```promql
(netobs_tcp_state_max_srtt_seconds > 0.2)
and
(
  netobs_tcp_state_min_cwnd
  < (avg_over_time(netobs_tcp_state_min_cwnd[5m] offset 5m) * 0.5)
)
```

- 200 ms 초과 srtt 단독은 cross-region 정상 트래픽 일 수 있어 cwnd backoff 와의 AND 로만 발화
- baseline 은 직전 5 분 윈도우 (offset 5m) 의 5 분 평균 으로 burst 직후 baseline 이 오염되지 않도록 산출
- `for: 2m` 으로 transient noise 차단

## 진단 워크플로

1. alert payload 의 `namespace` / `pod` / `node` 로 수신 Pod 식별
2. raw gauge 3 종 동시 조회

   ```sh
   PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
   for m in netobs_tcp_state_max_srtt_seconds netobs_tcp_state_min_cwnd netobs_tcp_state_min_ssthresh; do
     echo "--- $m"
     kubectl exec -n monitoring $PROM_POD -c prometheus -- \
       wget -qO- "http://localhost:9090/api/v1/query?query=${m}{namespace=\"NS\",pod=\"POD\"}" | jq
   done
   ```

3. 동일 Pod 의 send path 지표 (`netobs_pod_stage_latency_labeled_seconds_bucket` p99 stage=sendmsg_ret) 와 비교해 RTT 악화가 peer 측인지 본 Pod 측인지 분류
4. `netobs_retrans_events_labeled_total` rate 로 동일 Pod 의 재전송 발생 여부 확인
5. peer pod 에 대한 hint 가 필요하면 `netobs_drop_events_flow_total` (allow-list 등록 시) 의 src/dst 5-tuple 로 peer 식별

## 검증 시나리오

dev 클러스터에서 `iperf3` + `tc netem` 으로 합성 RTT inflation 을 주입해 alert 발화를 확인하는 절차다.

```sh
# 수신측 Pod (server) 에서 iperf3 listen
kubectl exec -n correlation-stress iperf-server -- iperf3 -s

# 송신측 Pod (client) 에서 tc netem 으로 100 ms RTT 추가 후 트래픽 발생
kubectl exec -n correlation-stress iperf-client -- sh -c '
  tc qdisc add dev eth0 root netem delay 100ms 20ms distribution normal
  iperf3 -c iperf-server -t 600
'
```

`netobs_tcp_state_max_srtt_seconds` 가 0.2 이상으로 상승하고 동일 시간대 `netobs_tcp_state_min_cwnd` 가 baseline 의 50 % 미만으로 떨어지면 2 분 후 alert 발화가 기대 동작이다.

## 카디널리티 분석

scrape 시점 시리즈 수 상한 산정.

- per gauge: `active receiving Pods` x `node` 의 곱. 단일 cluster 에서 active receiving Pods 가 50 이고 worker 3 노드면 50 x 3 = 150 시리즈
- 3 gauge x 150 = 450 시리즈 / scrape 가 worst case

receive path event 가 발생하지 않은 Pod 는 시리즈가 emit 되지 않아 실 운영에서는 더 적다. 추가 cardinality 가드는 두지 않으며, 위험 신호 (Pod 수 폭증) 가 보이면 Prometheus `count(netobs_tcp_state_min_cwnd) by (node)` 로 시리즈 수를 즉시 가시화한다.

## 알려진 한계

- IPv6 미지원: `tcp_v6_*` kprobe 와 v6 sock family 처리를 두지 않아 IPv6 connection 은 본 메트릭에 잡히지 않는다
- `tcp_v4_rcv` 미 emit: L3 entry 의 sock 미가용으로 stage counter 가 비어 있다. follow-up 에서 skb 헤더 파싱 기반 추출 검토
- rcv_established 의 emit 빈도: high-bandwidth flow (>10 Gbps) 에서 ringbuf 용량 초과 시 일부 sample 이 누락될 수 있다. follow-up 에서 sample 율 조정 검토
