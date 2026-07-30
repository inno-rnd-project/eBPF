# pod 식별자 표현 규약

이슈 #383의 pod 식별자 단일 규약이다. pod를 노출하는 응답의 표준은 `namespace`와 pod 이름의 분리 필드이며, 결합 표현(`ns/pod` 한 문자열)은 기본 식별자로 쓰지 않는다. 결합 표현이 이미 계약으로 존재하는 응답은 필드를 불변 유지하고 분리 필드를 additive로 병기한다(출력 호환). 프론트는 신규 소비에서 분리 필드를 쓰고 슬래시 파싱을 하지 않는다.

## 규약

1. 분리 필드가 표준이다. 신규 API는 `namespace`와 `pod`(이름)를 분리 필드로 노출한다.
2. 결합 표현이 필요하면 별도 id 필드로 병기한다. 기존에 결합값이 `pod`나 `top_pod` 이름을 점유한 응답은 그 필드를 결합 id 표현으로 규정해 불변 유지하고, pod 이름의 분리 필드는 `pod_name`으로 병기한다.
3. sentinel은 결합 표현에만 둔다. namespace 미상이면 결합 표현은 `_unknown/pod`를 유지하고, 분리 필드는 `namespace`를 생략(`omitempty`)해 "생략 = 미상"으로 해석한다.
4. 시계열 라벨 키 규약(`src_namespace`/`src_pod` 등)은 Prometheus 메트릭 계약이라 본 규약 대상이 아니다.

## API별 매핑

### 분리 표준 (namespace + pod가 이름)

| API | 필드 |
|---|---|
| `node/{node}/pods` | `namespace`, `pod` |
| `pod/{namespace}/{pod}` | `namespace`, `pod` |
| `/memory` | `namespace`, `pod` |
| `node-map` | `namespace`, `pod` |
| `inventory` | `namespace`, `pod` |
| `incidents` | `namespace`, `pod` |
| `gpu-rca` suspects | `namespace`, `pod` |
| `gpu-status` pods | `namespace`, `pod` |
| `gpu-idle` | `namespace`, `pod` |

### 결합 id 잔존 (결합 필드 불변 + 분리 필드 병기, #383)

| API | 결합 id 필드 | 분리 필드 |
|---|---|---|
| `/health` hotspot | `top_pod` | `top_pod_namespace`, `top_pod_name` |
| `/health` dominant_pressure | `pod` | `namespace`, `pod_name` |
| `/pressure?scope=pod` | `pod` | `namespace`, `pod_name` |
| `node/{node}` top_pods | `pod` | `namespace`, `pod_name` |

## 표시 지침 요약

- 신규 소비는 분리 필드 기준으로 신원을 읽고 결합 필드의 슬래시 파싱 코드를 제거
- 결합 id 필드는 표시용 단일 문자열이나 기존 화면 호환에만 사용
- 분리 필드에서 `namespace` 생략은 결측이 아니라 namespace 미상(계측 시점 의존)으로 해석
