# cluster-node-pod drill-down navigation

이슈 #87의 cluster-node-pod 계층 navigation 통합에 대한 운영자 가이드다. 6 dashboard (`observability-overview`, `netobs-overview`, `gpuobs-overview`, `correlation-overview`, `gpu-network-correlation`, `workload-injector`) 의 패널 단위 link 로 cluster → node → pod 흐름을 단일 click 으로 이동하게 한다. rca-summarizer 는 variable 미보유 라 역방향 진입점으로만 노출한다.

## 사용 시나리오

- cluster 이상 식별: `observability-overview`의 cluster health stat (GPU/CPU/Network/Memory) 에서 비정상 신호 발견 후 해당 dashboard 의 link 메뉴로 `netobs-overview` 또는 `gpuobs-overview` 로 이동
- node 단위 좁힘: `observability-overview`의 node-scope 패널 (Node cpu_throttle, memory_pressure, GPU utilization 등) 에서 특정 node 와 Pod 식별 후 link 메뉴로 pod-level dashboard 진입
- pod 단위 진단: pod-scope 패널 (Per-pod stage latency, GPU memory used 등) 에서 noisy neighbor 의심 시 correlation-overview 의 victim-suspect 표로 이동
- 부하 주입 추적: `workload-injector` 의 active 시계열 에서 부하 대상 (`target_pod`) 식별 후 correlation 의 victim 패널 또는 overview 의 src_pod 패널로 이동
- alert 진입: `observability-overview` 또는 `netobs-overview` 의 alert 관련 패널 에서 rca-summarizer 로 역방향 이동

## dashboard 별 출발지 / 도착지 매트릭스

| 출발 dashboard | 출발 panel scope | 도착 dashboard | 전파 variable |
|---|---|---|---|
| observability-overview | cluster (health stat, dominant cause, cluster 시계열) | netobs-overview | `node`, `src_pod`, time |
| observability-overview | cluster | gpuobs-overview | `node`, `src_pod`, time |
| observability-overview | cluster | correlation-overview | time |
| observability-overview | node (cpu/memory/gpu/drop 시계열, alert table) | netobs-overview | `node`, `src_pod`, time |
| observability-overview | node | gpuobs-overview | `node`, `src_pod`, time |
| observability-overview | node | correlation-overview | `victim_pod`, time |
| observability-overview | pod (stage p99, GPU mem, noisy neighbor) | netobs-overview | `node`, `src_pod`, time |
| observability-overview | pod | gpuobs-overview | `node`, `src_pod`, time |
| observability-overview | pod | correlation-overview | `victim_pod`, time |
| observability-overview | alert (Active alerts table, Firing bargauge) | rca-summarizer | time |
| netobs-overview | node (stage p99 send/recv, drop, retrans) | observability-overview, gpuobs-overview, correlation-overview | `node`, time |
| netobs-overview | drop (rate, top drop reasons) | rca-summarizer | time |
| netobs-overview | per-pod (stage latency p95) | gpuobs-overview, correlation-overview | `node`, `src_namespace`, `src_pod`, time |
| gpuobs-overview | device (util, mem, temp, power, throttle) | observability-overview, netobs-overview, correlation-overview | `node`, time |
| gpuobs-overview | per-pod (GPU mem, CUDA kernel, H2D/D2H) | netobs-overview, correlation-overview | `node`, `src_pod`, time |
| correlation-overview | victim-suspect table | netobs-overview, gpuobs-overview | `src_namespace`/`src_pod`, time |
| gpu-network-correlation | 단일 패널 | gpuobs-overview, netobs-overview, correlation-overview | `node`, `src_namespace`, `src_pod`, time |
| workload-injector | 4 패널 | correlation-overview, observability-overview, netobs-overview | `victim_*` / `src_*` (target에서 매핑), time |

## variable 매핑 변환 표

target dashboard 별로 variable 명이 다르므로 link URL 의 query string 에 매핑 변환을 적용한다.

| 출발 variable | 도착 dashboard | 도착 variable |
|---|---|---|
| `src_namespace` | netobs-overview, observability-overview, gpuobs-overview, gpu-network-correlation | `src_namespace` (직접) |
| `src_pod` | 동일 dashboard 군 | `src_pod` (직접) |
| `src_namespace` | correlation-overview | `victim_namespace` |
| `src_pod` | correlation-overview | `victim_pod` |
| `target_namespace` (injector) | correlation-overview | `victim_namespace` |
| `target_pod` (injector) | correlation-overview | `victim_pod` |
| `target_namespace` (injector) | netobs-overview, observability-overview | `src_namespace` |
| `target_pod` (injector) | 동일 | `src_pod` |
| `victim_namespace` (correlation) | netobs-overview, observability-overview, gpuobs-overview | `src_namespace` |
| `victim_pod` (correlation) | 동일 | `src_pod` |

## URL 표준

모든 drill-down link 의 URL 은 다음 표준을 따른다.

- `${variable:regex}` format spec 으로 multi-select variable 의 regex 인코딩 처리 (예: `var-node=${node:regex}` 가 `var-node=(node-a|node-b)` 인코딩)
- `${__url_time_range}` macro 부착으로 시간 범위 자동 전파 (`from=...&to=...` 자동 expand)
- `targetBlank: true` 로 새 탭 진입해 출발 dashboard 컨텍스트 유지

```text
/d/{target_uid}?var-{variable}=${{value:regex}}&{${__url_time_range}}
```

## `hide: 2` 받기 전용 variable 패턴

drill-down 으로 URL parameter 만 받고 UI selector 를 노출 하지 않는 케이스 가 있다. 본 이슈에서는 `correlation-overview` 의 `resource_dimension` variable 에 `hide: 2` 를 적용했다. 출발지 의 victim_pod 매칭만으로 dimension 자동 채워지므로 별도 UI 노이즈 차단.

URL 로 직접 진입 (link 없이) 하는 케이스 에서는 variable 의 `includeAll` 과 default value 로 자동 채워진다.

## 후속 dashboard 추가 시 체크리스트

후속 dashboard 신설 시 본 가이드를 spec 으로 따른다.

- panel 의 scope (cluster / node / pod / device) 식별
- 적절한 도착지 (다른 dashboard 의 동일 scope panel) 매핑
- variable 명 매핑 변환 표에 추가
- URL 표준 (`${variable:regex}` + `${__url_time_range}`) 준수
- 신규 link 의 verify.sh 매핑 표 갱신

## 비목표

- table row 클릭 의 동적 drilldown (`${__data.fields.X}`): Grafana 12.x 의 `panel.links` 에서 row scope 미지원. row 단위 동적 drilldown 이 필요하면 별도 grafana plugin 또는 dashboard 신설로 후속 이슈에 위임
- namespace 단위 dashboard 신설: namespace 와 node 가 직교 차원이라 cluster-node-pod 계층과 별개로 follow-up 이슈로 위임
- dashboard 자체 신설: 본 이슈는 link variable 통합만 수행하며 신규 dashboard 추가는 본 이슈 범위 밖
- bookmark / URL 호환성: overview 의 `pod` → `src_pod` rename 은 본 dashboard 가 production 진입 전이라 alias variable 미제공
