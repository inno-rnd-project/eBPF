# 대기 stage 꼬리 지연 비율 alert 운영 가이드

본 문서는 `NetObsHighWaitStageTailRatio` alert의 산정 방식, 임계의 근거, 대응 절차를 정리한다. 이 alert는 `NetObsHighStageLatencyP99`가 제외한 대기 stage(`ack_wait`, `rcv_app`, `connect`)의 비정상을 node 단위로 감지한다.

## 왜 p99 임계가 아니라 꼬리 비율인가

대기 stage의 지연은 로컬 처리 병목이 아니라 지연 ACK 타이머, 앱의 수신 버퍼 수거 주기, 핸드셰이크 왕복에 좌우된다. 그래서 정상 상태의 p99가 노드마다 최대 백 배 넘게 다르고(`rcv_app` 1.6ms~0.25s, `connect` 0.25~4ms), histogram 버킷이 2배 간격이라 p99가 버킷 단위로 튄다. 절대 p99 임계는 상시 오탐이 되고, 자기 baseline 대비 배수나 z-score는 ms 단위 baseline에서 조치할 것이 없는 장시간 발화를 만든다.

이 alert는 "stage별 경계를 넘은 이벤트의 비율"을 본다. 경계가 버킷 경계와 같아 보간 없이 정확히 세며, 경계마다 뜻이 분명하다.

| stage | 경계 | 경계를 넘는다는 뜻 |
|---|---|---|
| `connect` | 0.524288s | 초기 SYN 재전송 타이머(1s) 전후의 SYN 또는 SYN-ACK 재전송이 있었다 |
| `rcv_app` | 0.524288s | 수신 앱이 데이터를 0.5초 넘게 가져가지 않았다 |
| `ack_wait` | 1.048576s | 지연 ACK 상한(200ms)을 크게 넘겨 ACK가 실제로 보류됐다 |

`to_veth`는 대기 stage가 아니라 `tcp_sendmsg` 호출 안의 처리 구간이라 대상에서 뺐다([측정 범위](send-path-stage-latency.md)).

## 산정

recording rule은 `deploy/gpuobs/base/prometheus-rule-network.yaml`의 `netobs-gpuobs.wait-stage-tail.recording` 그룹(1분 간격)에 있다.

- `node:netobs_wait_stage_tail:rate5m`: 경계를 넘은 이벤트의 초당 발생률
- `node:netobs_wait_stage_tail_ratio:5m`: 전체 이벤트 대비 비율(0~1). 이벤트가 없는 구간은 시리즈 부재
- `node:netobs_wait_stage_tail_ratio:median7d`: 같은 node와 stage의 직전 7일 비율 중앙값. 이력이 1일을 넘어야 산출

alert 조건은 다음 세 가지가 10분 동안 이어지는 것이다.

- 꼬리 비율 1% 이상
- 꼬리 이벤트 0.01/s 이상 (5분에 3건, 이벤트 한두 건으로 비율이 튀는 것을 막는 게이트)
- 7일 중앙값 1% 미만 (평소에도 꼬리가 두꺼운 만성 상태는 억제)

입력은 pod 라벨이 없는 `netobs_stage_latency_seconds`다. workload 귀속과 무관하게 node 단위로 세므로 host 프로세스의 트래픽도 포함한다.

## 임계의 근거

2026-10-17부터 37일의 이력을 1분 간격으로 재생해 정했다. 조건이 10분 넘게 이어진 구간은 두 번이고 모두 실제 사건이었다.

- 10-26 gpu `ack_wait` 36분: Prometheus 장애 복구 직후
- 11-12 worker1 `connect` 13분: grafana의 SYN 재시도

3.4.8까지의 `ack_wait`는 payload 없는 순수 ACK도 기준점으로 삼아, 유휴 연결의 데이터 간격이 초 단위 꼬리로 섞일 수 있었다. 2026-11-24 gpu의 `ack_wait` 발화가 이 경우였고 3.4.9에서 기준점을 고쳤다([측정 범위](rcv-path-stage-latency.md)). 7일 중앙값 억제는 3.4.9 배포 뒤의 표본이 창의 과반이 될 때까지 gpu `ack_wait`에 남는다.

그 밖에는 gpu `connect` 7분(netem으로 손실을 건 실험)이 가장 길었고, 나머지 9개 node와 stage 조합은 조건을 한 번도 만족하지 않았다. `for: 10m`은 node API의 지속성 게이트(`ALERT_STATUS_MIN_HOLD` 기본 10m)와 같아, 발화 시점에 `node/{node}`와 `node-map`의 상태가 함께 바뀐다.

## 한계

- 10분 미만 사건은 잡지 않는다
- 경계 아래에서의 배수 상승(예: `connect` p99가 2ms에서 70ms로)은 잡지 않는다
- `connect`는 수립에 성공한 연결만 표본이 되고 10초를 넘긴 수립은 버려진다. SYN이 전부 차단되어 끝내 붙지 못하는 경우는 세지 않으며, 이는 `NetObsHighRetransmissionRate`와 drop alert가 맡는다
- `rcv_app`은 마지막 데이터 수신부터 10초 넘게 수거하지 않으면 꼬리에 들어가지 않고, 완전히 멈춘 앱은 표본이 없다. 감지 범위는 0.5초에서 10초 사이의 수거 지연이다
- 7일 중앙값은 rule 최초 로드 후 1일 뒤부터 산출되어, 그 전에는 만성 억제 없이 발화한다
- rule이 `le="0.524288"`과 `le="1.048576"` 버킷을 직접 고르므로 버킷 구성을 바꾸면 rule과 promtool 테스트를 함께 옮겨야 한다. `internal/netobs/metrics`의 단위 테스트가 두 경계의 존재를 가드한다

## 대응 절차

발화하면 해당 node가 warn으로 표시된다. 다음 순서로 범위를 좁힌다.

- 어느 workload인지 확인: `topk(5, sum by (src_namespace, src_workload, dst_workload, traffic_scope) (rate(netobs_stage_latency_labeled_seconds_count{node="<node>",stage="<stage>"}[5m]) - ignoring(le) rate(netobs_stage_latency_labeled_seconds_bucket{node="<node>",stage="<stage>",le="<경계>"}[5m])))`. workload 귀속 시리즈는 host 트래픽이 특정 pod로 잘못 귀속될 수 있으므로, 결과가 뜻밖의 pod를 가리키면 host 프로세스도 의심한다
- `connect`: 같은 node의 `NetObsHighRetransmissionRate`와 `netobs_pod_retrans_segments_total{kind="syn"}` 증가를 함께 본다. 대상 pod의 재시작, Service endpoint 교체, 일부 SYN이 유실되어 재시도 끝에 붙는 경로 문제가 흔한 원인이다
- `rcv_app`: 수신 pod의 CPU throttle(`pod:cpu_throttle_score:5m`)과 재시작 여부를 본다. 앱의 수거가 느려졌거나 스케줄링에서 밀린 경우다
- `ack_wait`: 수신측이 ACK를 내보내지 못한 경우라 수신 node의 softirq 부하와 수신 pod의 상태를 본다. 장애 복구 직후의 일시적 급증일 수 있다
- 며칠째 이어지면 7일 중앙값이 1%를 넘으면서 alert가 스스로 꺼진다. 이는 해소가 아니라 만성 상태로 분류된 것이므로 `node:netobs_wait_stage_tail_ratio:median7d`를 확인한다
