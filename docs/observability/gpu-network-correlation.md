# GPU x network cross-correlation 통합 패널

이슈 #86 의 GPU-network cross-correlation 통합 패널에 대한 운영자 가이드 다. 단일 timeseries 패널에 GPU device 시계열 2 종과 network Pod 시계열 2 종 그리고 correlation overlay 1 종을 같은 timeline 에 정렬해 GPU 유휴와 network pressure 의 상관 관계를 시각적으로 비교 가능하게 한다.

## 사용 시나리오

- GPU 사용률이 낮은데 (`node:gpu_util_ratio:5m` 0.2 미만) 같은 시점 네트워크 throughput 이 saturate 상태면 GPU 유휴의 원인이 network pressure 일 가능성이 높다
- GPU 메모리 사용 비율이 0.9 이상인데 throughput 이 낮으면 GPU 메모리 압박이 GPU 유휴의 원인일 가능성이 높다
- 두 도메인이 동시에 spike 면 correlation overlay 의 `correlation_noisy_neighbor_score` 와 dimension 라벨로 noisy neighbor 가능성을 확인

## scope 와 join 의 의미

본 패널은 두 도메인 시계열의 join key 를 엄밀히 매칭하지 않는다. dev cluster spike 결과 RTX 3090 단일 GPU 환경의 NVML 이 Pod scope GPU utilization 을 노출하지 않아 GPU 시계열은 device scope (`node`, `gpu_uuid`) 로 강등 적용된다. network 시계열은 Pod scope (`node`, `src_namespace`, `src_pod`, `src_pod_uid`) 그대로 유지된다. 두 도메인의 공유 라벨은 `node` 하나라 같은 노드의 같은 timeline 위에 정렬하는 것까지만 보장한다. Pod 인스턴스 단위 GPU 부하 분할은 본 패널 범위 밖이며 follow-up 이슈로 분리될 예정이다.

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

- Pod 인스턴스 단위 GPU 부하 분할 (NVML 비지원 환경 제약)
- 자동 인과 분석 (이슈 본문 비목표 그대로)
- alerting rule 신규 정의 (별도 이슈로 위임)
- 다중 victim Pod 동시 비교 (단일 `$src_pod` 선택만 지원)
- `$gpu_uuid` 기반 device 단위 drill-down (별도 follow-up 이슈로 분리 예정)

## 실패 진단

- 패널이 비어 있는 경우: recording rule 적용 후 5 분 warmup 이 끝났는지 확인. `kubectl get prometheusrule -n ebpf-project netobs-gpuobs-correlation` 로 rule 존재 확인
- GPU 시계열만 비어 있는 경우: `gpuobs-agent` DaemonSet 의 Pod 상태 확인. `gpuobs_device_utilization_percent` 가 prometheus 에 emit 되는지 확인
- network 시계열만 비어 있는 경우: 선택한 Pod 가 실제 트래픽을 발생시키는지 확인. `netobs_pod_bytes_total{src_pod="<pod>"}` 의 rate 가 0 보다 큰지 확인
- correlation overlay 가 비어 있는 경우: `correlation-exporter` 의 detection window 안에서 noisy neighbor 신호가 임계 이상이 아닐 수 있다. idle cluster 에서는 정상 동작이다
