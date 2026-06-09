# GPU x network cross-correlation 통합 패널

이슈 #86 의 GPU-network cross-correlation 통합 패널에 대한 운영자 가이드 다. 단일 timeseries 패널에 GPU device 시계열 2 종과 network Pod 시계열 2 종 그리고 correlation overlay 1 종을 같은 timeline 에 정렬해 GPU 유휴와 network pressure 의 상관 관계를 시각적으로 비교 가능하게 한다.

## 사용 시나리오

- GPU 사용률이 낮은데 (`node:gpu_util_ratio:5m` 0.2 미만) 같은 시점 네트워크 throughput 이 saturate 상태면 GPU 유휴의 원인이 network pressure 일 가능성이 높다
- GPU 메모리 사용 비율이 0.9 이상인데 throughput 이 낮으면 GPU 메모리 압박이 GPU 유휴의 원인일 가능성이 높다
- 두 도메인이 동시에 spike 면 correlation overlay 의 `correlation_noisy_neighbor_score` 와 dimension 라벨로 noisy neighbor 가능성을 확인

## scope 와 join 의 의미

본 패널의 device scope row 는 두 도메인 시계열의 join key 를 엄밀히 매칭하지 않는다. GPU 시계열은 device scope (`node`, `gpu_uuid`) 로 두고 network 시계열은 Pod scope (`node`, `src_namespace`, `src_pod`, `src_pod_uid`) 그대로 유지하므로 두 도메인의 공유 라벨은 `node` 하나라 같은 노드의 같은 timeline 위에 정렬하는 것까지만 보장한다. Pod 단위 정합 분석은 본 dashboard 의 별도 row 인 Pod 단위 multi-domain 분석 (#120) 에서 다룬다.

## Pod 단위 multi-domain 분석 (#120)

PR #104 의 `gpuobs_pod_utilization_percent` 와 기존 podbytes collector 의 `netobs_pod_bytes_total` 이 동일한 4 Pod 라벨 join key (`node`, `src_namespace`, `src_pod`, `src_pod_uid`) 를 공유하므로 두 도메인을 Pod 단위 로 직접 join 가능하다. 본 절은 dashboard 의 Pod scope row 의 panel 3 종 과 신규 recording rule `pod:gpu_network_correlation_score:5m` 의 의미를 정리한다.

### 신규 recording rule 의 정의

`pod:gpu_network_correlation_score:5m{node, src_namespace, src_pod, src_pod_uid}` 는 다음 두 factor 의 곱 형태 단일 score 다.

- GPU factor 는 `max by(node, src_namespace, src_pod, src_pod_uid) (pod:gpu_util_p95:5m) / 100` 으로 GPU 사용률을 0-1 비율로 정규화. `gpu_uuid` 차원은 Pod 가 다수 GPU 사용 시 worst GPU 채택 후 drop
- network factor 는 `clamp_max(pod:network_throughput_bps:5m / on(node) group_left() (netobs_node_nic_capacity_bytes_per_sec * 8), 1.0)` 으로 NIC capacity 점유율을 0-1 범위로 clamp. burst 시 score 폭주 차단

GPU factor 와 network factor 둘 다 0-1 범위라 곱 score 도 0-1 범위 다. score 0.3 이상 Pod 가 GPU heavy 와 network heavy 를 동시 만족 하는 후보 다.

### dashboard panel 3종

- panel 5 dual-axis 시계열: 좌축 GPU util percentunit, 우축 network throughput bps 와 network p99 latency seconds. 동일 Pod 의 두 도메인 추세를 한 차트에 시간 정렬
- panel 6 TopN 표: `topk(20, pod:gpu_network_correlation_score:5m)` 으로 상위 20 Pod 노출. score 컬럼은 color-background gradient 와 0.1/0.3/0.5 임계 색상 단계 적용
- panel 7 산점도: x 축 GPU util p95, y 축 network throughput bps 의 Pod 별 분포. 우상단 영역 의 Pod 가 GPU heavy 와 network heavy 동시 만족

### RTX 3090 의 MIG 미지원 환경 fallback 정책

dev cluster 의 RTX 3090 은 MIG 미지원 환경 이라 instance 단위 분석은 본 절 범위 밖이다. 단 `gpuobs_pod_utilization_percent` 가 raw 메트릭 부재 환경 (active CUDA workload 가 없는 dev cluster) 에서는 `pod:gpu_util_p95:5m` 가 0 series 가 되어 `pod:gpu_network_correlation_score:5m` 도 0 series 로 graceful empty 된다. dashboard 의 Pod scope panel 도 동일한 0 series fallback 으로 empty 표시 된다. recording rule 정의 자체는 RTX 3090 외 GPU 모델 (A100, H100 등) 환경에서 그대로 활성된다.

### 운영자 drilldown 흐름

panel 6 TopN 표에서 단일 Pod 선택 후 link menu 의 netobs overview dashboard 로 이동해 해당 Pod 의 network flow 와 stage latency 상세 확인. GPU 도메인 상세는 panel 1 의 device scope row 의 gpuobs overview link 로 이동.

## variable cascading

- `$node`: 단일 선택. GPU device 시계열과 network Pod 시계열 모두 본 변수로 필터
- `$src_namespace`: cascading. `$node` 선택에 따른 namespace 후보만 노출. default `All`
- `$src_pod`: cascading. `$node` 와 `$src_namespace` 선택에 따른 Pod 후보만 노출. default `All`

## axis 단위 정렬

panel 의 field config 가 query 단 변환 없이 Grafana native unit 으로 자동 SI prefix 표기를 처리한다.

- left axis (`percentunit`, 0-1 scale): GPU util, GPU mem used ratio, correlation overlay score
- right axis (`bps`, Grafana 자동 Mbps / Gbps 표기): network throughput
- right axis (`s`, Grafana 자동 ms / us 표기): network p99 latency

raw recording rule 의 단위를 그대로 활용하므로 단위 변경 시 query 재작성 부담이 없다.

## correlation overlay 의 해석

`correlation_noisy_neighbor_score{victim_pod=~"$src_pod",resource_dimension=~"gpu|network"}` 매칭으로 선택한 Pod 이 GPU 또는 network dimension 에서 noisy neighbor 신호를 보이는지 표시한다. `$src_pod` 는 항상 victim 으로 고정되며 suspect 축은 본 패널에서 노출하지 않는다. suspect 식별이 필요하면 drilldown link 의 correlation overview dashboard 로 이동해 victim-suspect 페어 테이블 을 참조 한다.

## sanity check

recording rule 적용 후 최소 5 분의 sample 누적이 필요하다. 5 분 warmup 이후 다음 두 표현식의 5 분 평균이 ±5% 오차 범위에서 일치하면 recording rule 산정이 정상이다.

- `node:gpu_util_ratio:5m{node="<node>"}` 의 5 분 평균
- `avg_over_time(gpuobs_device_utilization_percent{node="<node>"}[5m]) / 100` 의 동일 시점 값

network 도메인은 raw `netobs_pod_bytes_total` 의 `rate([5m]) * 8` 합산과 `pod:network_throughput_bps:5m` 가 동일 라벨 셋에서 일치해야 한다.

## drilldown 흐름

패널 우측 상단의 link menu 에서 세 overview dashboard 로 이동한다. 각 link 는 현재 선택된 variable 을 query string 으로 전달해 target dashboard 의 동일 컨텍스트 (Pod, namespace, node) 가 자동 적용된다.

- GPU 시계열 상세는 `gpuobs-overview` (variable: `node`, `src_pod`)
- network 시계열 상세는 `netobs-overview` (variable: `node`, `src_namespace`, `src_pod`)
- correlation 페어 테이블은 `correlation-overview` (variable 매핑: `$src_namespace` 가 `victim_namespace`, `$src_pod` 가 `victim_pod` 로 변환)

## 비목표

- Pod 인스턴스 단위 GPU 부하 분할의 NVML 직접 지원 (RTX 3090 환경 제약. #120 에서 Pod 단위 join score 도입으로 합성 score 흐름은 cover 되었으나 NVML 의 instance scope 직접 노출은 여전히 본 PR 범위 밖)
- 자동 인과 분석 (이슈 본문 비목표 그대로)
- alerting rule 신규 정의 (별도 이슈로 위임)
- 다중 victim Pod 동시 비교 (단일 `$src_pod` 선택만 지원)
- MIG instance 단위 cross-correlation (dev cluster RTX 3090 MIG 미지원으로 인터페이스 수준만 유지)
- DCGM exporter 통합 (별도 이슈)

## 실패 진단

- 패널이 비어 있는 경우: recording rule 적용 후 5 분 warmup 이 끝났는지 확인. `kubectl get prometheusrule -n ebpf-project netobs-gpuobs-correlation` 로 rule 존재 확인
- GPU 시계열만 비어 있는 경우: `gpuobs-agent` DaemonSet 의 Pod 상태 확인. `gpuobs_device_utilization_percent` 가 prometheus 에 emit 되는지 확인
- network 시계열만 비어 있는 경우: 선택한 Pod 가 실제 트래픽을 발생시키는지 확인. `netobs_pod_bytes_total{src_pod="<pod>"}` 의 rate 가 0 보다 큰지 확인
- correlation overlay 가 비어 있는 경우: `correlation-exporter` 의 detection window 안에서 noisy neighbor 신호가 임계 이상이 아닐 수 있다. idle cluster 에서는 정상 동작이다
