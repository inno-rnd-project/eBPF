# deploy/swagger-ui

이슈 #100 의 자체 dashboard용 REST API layer 의 통합 Swagger UI 진입점이다. `swaggerapi/swagger-ui` 공식 image의 `URLS` 환경 변수로 4 agent (correlation-exporter와 netobs-agent와 gpuobs-agent) 의 `/api/v1/swagger.json` spec을 dropdown으로 묶어 운영자가 한 곳에서 endpoint를 탐색 가능하게 한다.

## 적용

```sh
kubectl apply -k deploy/swagger-ui/
```

## 접속 방법

cluster 내부 또는 port-forward로 swagger-ui Service에 접속한다.

```sh
kubectl port-forward -n ebpf-project svc/swagger-ui 8080:8080
```

브라우저에서 `http://localhost:8080` 접속 후 상단 dropdown에서 agent를 선택해 endpoint 탐색 가능하다.

## agent 별 직접 접근

특정 agent의 endpoint만 빠르게 확인할 때는 agent의 자체 swagger UI를 사용한다.

```sh
kubectl port-forward -n ebpf-project svc/correlation-exporter 9830:9830
# 브라우저로 http://localhost:9830/api/v1/swagger/ 접속
```

## URLS 환경 변수 형식

`URLS` 는 swagger-ui 의 multi-spec dropdown source 다. JSON 배열로 각 spec의 `url` 과 `name` 을 지정한다.

```json
[
  {"url":"http://correlation-exporter.ebpf-project.svc.cluster.local:9830/api/v1/swagger.json","name":"correlation"},
  {"url":"http://netobs-agent.ebpf-project.svc.cluster.local:9810/api/v1/swagger.json","name":"netobs"},
  {"url":"http://gpuobs-agent.ebpf-project.svc.cluster.local:9820/api/v1/swagger.json","name":"gpuobs"}
]
```

신규 agent의 API 합류 시 본 배열에 1 항목 추가만으로 통합된다.
