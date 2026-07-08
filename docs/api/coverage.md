# API 커버리지 매핑

프로젝트 목표의 기능 항목별로 correlation-exporter REST API 의 커버리지를 매핑한 문서다. 프론트엔드가 "어떤 목표 기능을 어느 엔드포인트로 구현하는가" 를 판단하는 계약으로 쓰이며, 엔드포인트나 태그가 바뀌면 본 문서와 `cmd/correlation-exporter/main.go` 의 `@tag` 선언, 핸들러의 `@Tags` 를 함께 갱신한다.

## 분류 태그 9종

| 태그 | 도메인 | 엔드포인트 |
|---|---|---|
| `meta` | API 상태와 클러스터 헬스 요약 | `/api/v1/health` |
| `inventory` | 노드와 파드 인벤토리 | `/api/v1/nodes`, `/api/v1/pods` |
| `network` | 지연 단계 분해, 패킷 drop, pod 간 flow | `/api/v1/latency-breakdown`, `/api/v1/drops`, `/api/v1/flows` |
| `interference` | 자원 압박 랭킹과 노드 상세, 이벤트와 발화 이력, 메모리, 간섭 토폴로지 | `/api/v1/pressure`, `/api/v1/node/{node}`, `/api/v1/events`, `/api/v1/incidents`, `/api/v1/memory`, `/api/v1/topology` |
| `impact` | 간섭 상관 top-N 과 영향 전파 그래프 | `/api/v1/noisy-neighbor`, `/api/v1/cross-node-interference`, `/api/v1/service-impact`, `/api/v1/cross-level`, `/api/v1/impact-graph`, `/api/v1/impact-paths` |
| `gpu` | GPU 유휴 원인 분석 | `/api/v1/gpu-idle` |
| `trends` | 진단 신호 시계열 추이 | `/api/v1/trends` |
| `rca` | alert 별 root cause analysis 요약 (rca-summarizer 프록시) | `/api/v1/rca` |
| `playbook` | 원인 식별자별 대응 안내 정적 카탈로그 | `/api/v1/playbooks` |

## 목표 대비 커버리지

| 목표 항목 | 엔드포인트 (태그) | 판정 |
|---|---|---|
| 네트워크 지연 커널 단계 분석 | `latency-breakdown` (network). stage 별 p99 와 비중, 지배 단계. `ack_wait` 등 수신 경로 포함 | 커버 |
| 패킷 drop 지점·원인·시점 특정 | `drops` (network). reason 과 category 와 `drop_stage`, node/pod 귀속, 5-tuple last_seen, kernel stack | 커버 |
| 워크로드별 간섭 Top-N | `noisy-neighbor` (impact), `pressure` (interference) | 커버 |
| node 와 pod 단위 정보 수집 | `nodes`, `pods` (inventory), `node/{node}` (interference) | 커버 |
| 피어슨 상관계수 스코어링 | `noisy-neighbor`, `cross-node-interference`, `service-impact` (impact) 가 `correlation_*_score` 와 `causal_strength` 노출 | 커버 |
| 특정 node/pod 의 서비스 영향 분석 | `service-impact`, `impact-graph`, `impact-paths`, `cross-level` (impact) | 커버 |
| GPU 유휴 원인 (하드웨어 vs 네트워크) | `gpu-idle` (gpu). dominant cause 9종 | 커버 |
| Pod 간 네트워크 flow 추적 | `flows` (network), `topology` (interference) | 커버 |
| GPU 일반 자원 현황 (사용률·메모리·전력·온도) | `gpu-status` (gpu). `gpuobs_device_*` 와 `gpuobs_pod_*` 기반 | 커버 |
| Pod 별 RX/TX 대역폭 | `bandwidth` (network). `netobs_pod_bytes_total` 기반, allow-list 무관 전 pod 커버 | 커버 |
| 자원 사용량·지연 시계열 추이 | `trends` (trends). 간섭 4종 + 자원·지연 7종 시그널 | 커버 |
| GPU–네트워크 통합 분석 | `gpu-idle` + `pressure` + `bandwidth` 조합 | 프론트 합성 |
| 이벤트 발화 이력 (시간축) | `incidents` (interference). `ALERTS` range 합성으로 기간 내 발화 에피소드 (firing/resolved, 시작·종료 시각). `starts_at`을 `at` 파라미터에 결합해 사건 시점 재구성 진입점 | 커버 |
| 이벤트의 종합 원인 서사 (RCA) | `rca` (rca). Alertmanager webhook 으로 생성된 alert 별 RCASummary 를 프록시. 지배 차원과 최우선 의심 pod, 근거 메트릭, 신뢰도 | 커버 |
| 원인별 대응 안내 (playbook) | `playbooks` (playbook). gpu-idle cause 와 drop stage, dimension, alertname 별 확인 절차와 권고 조치. `cause` 단일 조회와 alertname 별칭 매칭, `at` 결합 링크 | 커버 |
| 통합 대시보드·알림 표시 | 프론트엔드 영역 | 범위 외 |
| 부하테스트 자동화 | `workload-injector` (dev 전용 LoadScenario CRD) 가 담당 | 범위 외 |

## 접근 제어

correlation-exporter API 는 무인증이므로 클러스터 내부 전용으로 운용하며 공인 노출을 금지한다. Service 는 `type: ClusterIP` 로 고정하고 NodePort 나 LoadBalancer 로 승격하지 않으며, 노드에서 socat 같은 임시 프록시로 공인 IP 에 노출하는 것도 금지한다. 접근은 NetworkPolicy 가 강제하며 허용 출처는 다음과 같다.

- `ebpf-project` 네임스페이스 내부 pod (rca-summarizer 와 workload-injector 와 swagger-ui)
- `monitoring` 네임스페이스 (Prometheus scrape)
- `observability.netobs/api-consumer: "true"` 라벨을 부여한 네임스페이스. 프론트엔드 대시보드 등 새 소비처는 해당 네임스페이스에 이 라벨을 추가해 허용한다

외부 노출이 요건이 되는 시점에는 토큰이나 mTLS 기반 인증 도입과 함께 별도 이슈로 재검토한다.

## 스펙 생성 규약

swagger 스펙은 swaggo 어노테이션이 단일 소스다. 임베디드 스펙(`internal/correlation/api/docs/`)과 통합본(`docs/api/openapi.yaml`)은 모두 `make swag-init` 과 `make swag-merge` 의 생성물이며, 재생성 누락은 `make check-swagger-drift` 가 감지한다.
