# rest-api e2e 검증 시나리오

이슈 #100의 REST API layer가 dev cluster에서 정상 emit되는지 회귀 가드한다. dev cluster 전용이며 prod에서는 실행하지 않는다.

## 사전 조건

- 4 agent (`correlation-exporter`와 `netobs-agent`와 `gpuobs-agent`와 `rca-summarizer`) 의 신규 image가 dev cluster에 배포된 상태
- `kubectl apply -k deploy/swagger-ui/` 로 통합 swagger-ui Pod 배포 (선택적, 미배포 시 본 가드는 swagger-ui 부분만 skip)

## 시나리오 개요

- 1차 가드: 4 endpoint 의 HTTP 200 응답과 `application/json` Content-Type 확인
- 2차 가드: 3 agent의 `/api/v1/swagger.json` HTTP 200 응답 확인
- 3차 가드: 통합 `swagger-ui` Service 의 `/` 경로 HTTP 200 응답 확인 (미배포 시 skip)

## 실행

```sh
./verify.sh
```

## 종료 코드

- 0: 검증 통과
- 1: 검증 실패 (endpoint 부재와 swagger.json 부재와 swagger-ui Service 응답 실패 등)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `API_NAMESPACE` | `ebpf-project` | agent와 swagger-ui Service의 namespace |

## 실패 시 진단

- `[fail] {svc}: ClusterIP 발견 불가` 로 떨어지면 agent Service 배포 여부 점검 (`kubectl get svc -n ebpf-project`)
- `[fail] {svc} {path}: HTTP {code}` 로 떨어지면 agent의 신규 image 적용 여부와 `make image-push-all` 실행 여부 확인
- `[fail] {svc} {path}: Content-Type 이 application/json 아님` 으로 떨어지면 handler 의 `WriteJSON` 호출 누락 확인
- `[skip] swagger-ui Service 미배포` 는 통합 swagger-ui Pod 미배포 상태. `kubectl apply -k deploy/swagger-ui/` 적용 후 재실행
