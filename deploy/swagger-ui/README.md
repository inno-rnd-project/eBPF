# deploy/swagger-ui

이슈 #100 의 자체 dashboard용 REST API layer 의 통합 Swagger UI 진입점이다. `swaggerapi/swagger-ui` 공식 image의 `URLS` 환경 변수로 4 agent (correlation-exporter와 netobs-agent와 gpuobs-agent) 의 `/api/v1/swagger.json` spec을 dropdown으로 묶어 운영자가 한 곳에서 endpoint를 탐색 가능하게 한다.

## 적용

```sh
kubectl apply -k deploy/swagger-ui/
```

## 접속 방법

### cluster 내부 접근 (권장)

cluster 내부 Pod 또는 ingress 경유 접속이 가장 정상 동작한다. swagger-ui Pod 가 dropdown 선택 시 agent 의 swagger.json 을 fetch 하는데, 이때 cluster DNS (`correlation-exporter:9830` 같은 숏네임) 가 해석 가능해야 한다.

### port-forward 접근 (한계 있음)

`kubectl port-forward svc/swagger-ui 8080:8080` 으로 로컬 브라우저에서 접속 가능하지만 dropdown 에서 spec 을 선택하면 로드 실패한다. 로컬 브라우저는 cluster 내부 DNS 를 해석할 수 없기 때문이다.

```sh
kubectl port-forward -n ebpf-project svc/swagger-ui 8080:8080
# 브라우저로 http://localhost:8080 접속하면 swagger UI 자체는 표시되지만
# dropdown 에서 agent 선택 시 swagger.json fetch 실패 (cluster DNS 미해석)
```

이 한계를 해소하려면 agent 별 직접 port-forward 후 자체 swagger UI 접근 (아래) 또는 cluster 내부 ingress 구성이 필요하다.

## agent 별 직접 접근

특정 agent의 endpoint만 빠르게 확인할 때는 agent의 자체 swagger UI를 사용한다.

```sh
kubectl port-forward -n ebpf-project svc/correlation-exporter 9830:9830
# 브라우저로 http://localhost:9830/api/v1/swagger/ 접속
```

## URLS 환경 변수 형식

`URLS` 는 swagger-ui 의 multi-spec dropdown source 다. JSON 배열로 각 spec 의 `url` 과 `name` 을 지정한다. 동일 namespace 의 Service 숏네임 으로 접근해 namespace 하드코딩을 회피한다.

```json
[
  {"url":"http://correlation-exporter:9830/api/v1/swagger.json","name":"correlation"},
  {"url":"http://netobs-agent:9810/api/v1/swagger.json","name":"netobs"},
  {"url":"http://gpuobs-agent:9820/api/v1/swagger.json","name":"gpuobs"}
]
```

신규 agent 의 API 합류 시 본 배열에 1 항목 추가만으로 통합된다. 본 swagger-ui Pod 가 ebpf-project 외 다른 namespace 로 배포 되어도 동일 namespace 의 agent Service 를 가리키므로 별도 수정 불필요.
