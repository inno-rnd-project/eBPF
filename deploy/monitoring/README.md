# deploy/monitoring

kube-prometheus-stack 의 운영 설정 patch 를 모은 디렉토리다. 본 디렉토리의 patch는 monitoring stack 자체의 declarative 정의가 아니라 application observability 영역에서 요구하는 특정 설정 (예: retention, ServiceMonitor selector) 을 cluster 에 반영하는 helper 이다.

## prometheus retention 60d 상향

이슈 #88 의 capacity-trends 패널이 4주 heatmap 과 30일 baseline offset 윈도우를 산정하므로 최소 60일치 sample 누적이 필요하다. kube-prometheus-stack 의 기본 retention 10일과 정합 안 되므로 본 patch 로 60일 상향 적용한다.

```sh
kubectl apply -k deploy/monitoring/
```

또는 직접 patch:

```sh
kubectl patch prometheus -n monitoring kube-prometheus-stack-prometheus \
  --type merge -p '{"spec":{"retention":"60d"}}'
```

## 사전 점검

- prometheus PV 의 가용 용량이 retention 확장 분량을 수용 가능한지 확인 (`kubectl exec -n monitoring prometheus-kube-prometheus-stack-prometheus-0 -c prometheus -- df -h /prometheus`)
- 60일 retention 의 디스크 사용량 추정은 현재 사용량 × (60/10) 비례 (단순 추정, 실제는 sample churn 에 따라 변동)
