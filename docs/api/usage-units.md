# memory·cpu 사용량 표현 규약

이슈 #382의 사용량 표현 단일 규약이다. memory 사용량이 API마다 `memory_percent`와 `memory_working_set_bytes`와 `memory_used_bytes`와 `memory_used_ratio`와 `memory_bytes`로 흩어져 있고 limit 유무에 따라 percent와 절대량이 갈려, 소비자가 열마다 단위를 추론해야 했다. 본 문서가 절대량+상대량 규약과 기존 필드의 매핑을 확정하며, 신규 필드는 본 규약을 따른다. 기존 필드의 rename과 제거는 없다(출력 호환). pressure와 health 등 score 계열(0~1 합성 점수)은 사용량이 아니라 본 규약 대상이 아니다.

## 규약

1. 절대량은 항상 제공한다. memory 사용량은 working set bytes, cpu 사용량은 5분 rate cores가 기준 개념이며, 신규 필드명은 `memory_working_set_bytes`와 `cpu_usage_cores`를 그대로 재사용한다. 절대량은 limit 유무와 무관하게 전 pod와 전 노드에서 산출되므로 프론트 비교 축이 된다.
2. 상대량은 분모가 있을 때만 additive로 병기한다(`omitempty` pointer). 신규 상대량 필드명은 분모 토큰을 포함해 자명하게 한다.
   - limit 분모: `*_limit_percent`(0~100) 또는 `*_limit_ratio`(0~1)
   - allocatable 분모: `*_allocatable_percent`(0~100) 또는 `*_allocatable_ratio`(0~1)
   - total 분모(GPU 메모리 등): `*_total_ratio`(0~1)
3. 스케일 접미사를 고정한다. `_percent`는 0~100, `_ratio`는 0~1이다.
4. 프론트 표시 계약: 절대량을 기본 표시로 쓰고 상대량은 분모가 존재하는 행에만 보조로 병기한다. 같은 열에 percent와 bytes를 혼용하지 않는다. limit 없는 pod는 상대량 필드가 생략되므로 "생략 = 분모 없음"으로 해석하고 결측 차원 판정은 `unmeasured_dimensions`(#378)를 쓴다.

## 기존 필드 매핑

같은 개념의 상이한 이름을 아래 표로 규약에 매핑한다. 신규 API는 표의 "규약 필드명"을 쓴다.

### 절대량 (항상 제공)

| API | 필드 | 개념 | 단위 | 규약 필드명 대응 |
|---|---|---|---|---|
| `node/{node}/pods`, `pod/{ns}/{pod}` | `cpu_usage_cores` | pod CPU 사용량 (5m rate) | cores | 규약 그대로 |
| `node/{node}/pods`, `pod/{ns}/{pod}` | `memory_working_set_bytes` | pod working set | bytes | 규약 그대로 |
| `node-vitals` | `cpu_usage_cores` | 노드 내 pod 합산 CPU 사용량 (5m rate) | cores | 규약 그대로 (#382 추가) |
| `node-vitals` | `memory_working_set_bytes` | 노드 내 pod 합산 working set | bytes | 규약 그대로 (#382 추가) |
| `/memory` | `working_set_bytes`, `rss_bytes`, `cache_bytes`, `swap_bytes` | pod 메모리 구성 요소 | bytes | `memory_working_set_bytes`의 무접두 변형 (memory 전용 API라 접두 생략) |
| `/memory` | `limit_bytes` | pod memory limit (분모값) | bytes | 분모 절대량 |
| `pod/{ns}/{pod}` | `limit_cores` | pod CPU limit (분모값) | cores | 분모 절대량 |
| `node/{node}/resources` | `usage` | 리소스별 사용량 | cpu cores, memory bytes, pods 개수 | 리소스 키가 단위를 결정하는 map 구조 |
| `node/{node}/resources` | `capacity`, `allocatable`, `requests`, `limits` | 리소스별 총량과 예약 | 동상 | 분모 절대량 |
| `inventory` | `cpu`, `memory_bytes` | 노드 capacity | cores, bytes | 분모 절대량 (사용량 아님) |
| `gpu-status`, `node-vitals` | `memory_used_bytes`/`gpu_memory_used_bytes`, `memory_total_bytes`/`gpu_memory_total_bytes` | GPU 메모리 사용량과 총량 | bytes | GPU 축 절대량 |

### 상대량 (분모 존재 시 병기)

| API | 필드 | 분모 | 스케일 | 비고 |
|---|---|---|---|---|
| `node/{node}/pods`, `pod/{ns}/{pod}` | `cpu_percent`, `memory_percent` | pod limit | 0~100 | limit 없는 pod 생략 |
| `node-vitals` | `cpu_percent`, `memory_percent` | 노드 allocatable | 0~100 | pod 축 동명 필드와 분모가 다름 (#313), 신규 필드라면 `*_allocatable_percent`에 해당 |
| `node-vitals` | `gpu_memory_percent` | GPU memory total | 0~100 | |
| `/memory` | `oom_risk` | pod memory limit | 0~1 | working_set/limit, 신규 필드라면 `memory_limit_ratio`에 해당 |
| `node/{node}/resources` | `utilization_ratio` | 노드 allocatable (gpu는 device 사용률 평균) | 0~1 | |
| `gpu-status` | `memory_used_ratio` | `memory_total_bytes` | 0~1 | |
| `gpu-rca` | `suspect_memory_limit_ratio` | pod memory limit | 0~1 | 분모 토큰 포함 명명의 선례 |
| `gpu-rca` | `node_memory_used_ratio` | 노드 MemTotal | 0~1 | |

## 표시 지침 요약

- 목록 화면의 사용량 열은 절대량(`cpu_usage_cores`, `memory_working_set_bytes`) 기준 단일 단위로 구성
- 상대량은 배지나 보조 텍스트로 병기하고 분모(limit 또는 allocatable)를 툴팁 등으로 명시
- limit 없는 pod에서 상대량 생략은 결측이 아니라 분모 부재이며, 근접도 의미가 필요하면 pressure 계열 API를 사용
