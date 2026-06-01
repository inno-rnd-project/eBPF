# REST API layer 운영자 가이드

이슈 #100의 자체 dashboard용 REST API layer에 대한 운영자 가이드다. 4 agent (`correlation-exporter`와 `netobs-agent`와 `gpuobs-agent`와 `rca-summarizer`) 가 도메인별 JSON endpoint를 노출해 Prometheus query 의존 없이 자체 dashboard에서 본 시스템의 분석 결과를 활용 가능하게 한다.

## 사용 시나리오

운영자가 자체 dashboard에서 다음 3 단계로 데이터를 활용한다.

- 1단계 API 탐색: 통합 Swagger UI (`kubectl port-forward svc/swagger-ui 8080:8080`) 또는 agent별 자체 UI (`/api/v1/swagger/index.html`) 에서 endpoint와 query parameter와 응답 schema 확인
- 2단계 데이터 조회: curl 또는 HTTP client로 endpoint 호출. limit과 offset으로 pagination
- 3단계 dashboard 시각화: JSON 응답을 자체 dashboard 의 그래프와 표로 변환

## endpoint 일람

| Agent | Endpoint | 용도 | 데이터 source 연결 상태 |
|---|---|---|---|
| `correlation-exporter` | `/api/v1/noisy-neighbor` | victim과 suspect와 dimension 별 noisy neighbor top-N | 완료 (Snapshot 직접 연결) |
| `netobs-agent` | `/api/v1/flows` | 5-tuple 기준 flow bytes와 rate | skeleton (실 source는 follow-up 이슈로 위임) |
| `netobs-agent` | `/api/v1/drops` | drop_reason 별 5-tuple 분포 | skeleton (실 source는 follow-up 이슈로 위임) |
| `gpuobs-agent` | `/api/v1/gpu` | device와 pod scope GPU 자원 사용량 | skeleton (실 source는 follow-up 이슈로 위임) |
| `rca-summarizer` | `/rca?alert=<name>` | alert별 RCA 요약 | 완료 (기존 #71 구현) |

## Swagger UI 접근

본 시스템의 Swagger UI는 두 가지 진입점을 제공한다.

### 통합 진입점 (운영자 권장)

`deploy/swagger-ui/` 의 Pod가 4 agent 의 `/api/v1/swagger.json` spec을 dropdown으로 묶는다. 운영자가 한 브라우저 창에서 4 agent의 모든 endpoint를 탐색 가능하다.

```sh
kubectl apply -k deploy/swagger-ui/
kubectl port-forward -n ebpf-project svc/swagger-ui 8080:8080
# 브라우저로 http://localhost:8080 접속 후 상단 dropdown 선택
```

### agent 별 직접 진입 (개발 편의)

특정 agent의 endpoint만 빠르게 확인할 때.

```sh
# correlation-exporter
kubectl port-forward -n ebpf-project svc/correlation-exporter 9830:9830
# 브라우저로 http://localhost:9830/api/v1/swagger/ 접속

# netobs-agent
kubectl port-forward -n ebpf-project svc/netobs-agent 9810:9810
# 브라우저로 http://localhost:9810/api/v1/swagger/ 접속

# gpuobs-agent
kubectl port-forward -n ebpf-project svc/gpuobs-agent 9820:9820
# 브라우저로 http://localhost:9820/api/v1/swagger/ 접속
```

## 응답 표준

### 200 OK (list endpoint)

```json
{
  "items": [
    {
      "victim": {"namespace": "ns-a", "pod": "victim-1", "pod_uid": "uid-1"},
      "suspect": {"namespace": "ns-b", "pod": "suspect-1", "pod_uid": "uid-2"},
      "dimension": "cpu",
      "rank": 1,
      "score": 0.85,
      "lag_steps": 0
    }
  ],
  "page": {
    "limit": 100,
    "offset": 0,
    "total": 1
  }
}
```

### 400 Bad Request

```json
{
  "error": {
    "code": "invalid_dimension",
    "message": "dimension 은 cpu / memory / network / gpu 중 하나여야 합니다"
  }
}
```

## pagination 표준

모든 list endpoint는 `limit` 과 `offset` 쿼리 파라미터를 지원한다.

- `limit` 기본 100, 최대 1000. 음수 또는 0 입력 시 기본값 적용
- `offset` 기본 0. 음수 입력 시 0 으로 보정
- 응답의 `page.total` 이 필터 적용 후 전체 결과 개수. 클라이언트는 `(offset + limit) >= total` 이면 추가 페이지 없음으로 판단

## 자체 dashboard 활용 예시

### curl

```sh
curl 'http://correlation-exporter.ebpf-project.svc.cluster.local:9830/api/v1/noisy-neighbor?dimension=gpu&limit=10'
```

### Python httpx

```python
import httpx

base = "http://correlation-exporter.ebpf-project.svc.cluster.local:9830"
resp = httpx.get(f"{base}/api/v1/noisy-neighbor", params={"dimension": "gpu", "limit": 10})
data = resp.json()
for item in data["items"]:
    print(item["victim"]["pod"], item["suspect"]["pod"], item["score"])
```

### JavaScript fetch

```javascript
const base = "http://correlation-exporter.ebpf-project.svc.cluster.local:9830";
const params = new URLSearchParams({dimension: "gpu", limit: 10});
const resp = await fetch(`${base}/api/v1/noisy-neighbor?${params}`);
const data = await resp.json();
data.items.forEach(item => {
  console.log(item.victim.pod, item.suspect.pod, item.score);
});
```

## 후속 endpoint 추가 시 체크리스트

신규 endpoint 추가 시 다음 절차로 본 표준을 따른다.

- `internal/<agent>/api/handler.go` 에 핸들러 함수 추가 (swaggo 주석 포함)
- handler 입력 validation과 pagination 표준 (`apicommon.ParsePagination`) 적용
- 응답 schema는 `<Domain>ListResponse` 형태로 정의 (items + PageInfo)
- 에러 응답은 `ErrorResponse` 표준 사용
- `make swag-init` 실행으로 swagger.json 자동 갱신
- 본 가이드의 endpoint 일람 표에 1 행 추가
- `deploy/swagger-ui/deployment.yaml` 의 URLS 환경 변수는 신규 agent 합류 시에만 변경 필요

## 비목표

- WebSocket 실시간 streaming endpoint는 본 PR 범위 밖. 별도 follow-up 이슈로 위임
- GraphQL endpoint는 본 PR 범위 밖으로 REST JSON 만 한정
- 인증과 인가 (RBAC) 는 cluster 내부 통신 가정으로 본 PR 미구현. 외부 노출 시 별도 보안 layer 필요
- flow와 drop과 gpu endpoint 의 실 source 연결은 본 PR 의 follow-up 이슈로 위임. 현재 skeleton 상태라 nil source 시 빈 응답 반환
- 통합 OpenAPI YAML (`docs/api/openapi.yaml`) 의 build time 자동 생성은 `make swag-merge` 타겟으로 보조 제공이며 cluster의 swagger-ui Pod는 각 agent의 spec을 직접 dropdown으로 호스팅하므로 통합 YAML 의 필요성은 보조적
