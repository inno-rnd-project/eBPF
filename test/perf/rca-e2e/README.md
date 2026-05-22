# rca-e2e 검증 시나리오

이슈 #71 수용 조건 2번 (workload-injector cpu kind 발화 시 dominant_dimension 이 cpu 로 식별)
의 자동화된 e2e 검증이다. dev cluster 한정으로 사용하며 prod 에서는 실행하지 않는다.

## 사전 조건

- dev cluster 의 `rca-summarizer` Deployment 가 ready 상태
- `correlation-exporter`, `netobs-agent`, `gpuobs-agent` Deployment / DaemonSet 가 ready
- `workload-injector:0.4.8` 이미지가 dev 노드에 로컬 build 되어 있음 (imagePullPolicy=Never)
- `correlation-stress` namespace 의 `victim` pod 가 존재

## 실행

```sh
./verify.sh
```

본 스크립트는 cpu kind 부하 인가 후 60 초 안에 `GPUIdleWithCPUThrottle` alert 에 대한
RCA 요약 응답의 `dominant_dimension` 필드가 `cpu` 인지 확인한다. 마지막에는 메모리 규칙에
따라 injector Job 을 정리한다.

## 종료 코드

- 0: 검증 통과
- 1: 검증 실패 (응답 timeout, dominant_dimension mismatch, rca-summarizer unreachable 등)
