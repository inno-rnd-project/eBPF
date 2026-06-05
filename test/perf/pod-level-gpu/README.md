# pod-level-gpu e2e 회귀 가드 (#104)

이슈 #104 의 Pod-level GPU utilization 도입을 dev cluster 에서 검증하는 회귀 가드 스크립트다. 3 단계 가드로 self-health 메트릭 발행, Pod utilization 시리즈 emit, recording rule 산출을 순차 확인한다. MIG 활성 환경의 instance level 검증은 dev cluster 환경 미보유로 본 가드 범위 밖이며 graceful degradation 경로 (mig_mode=unsupported) 만 cover 한다.

## 실행

```sh
test/perf/pod-level-gpu/verify.sh
```

스크립트가 자동으로 `pytorch-conv2d-bench` 워크로드를 적용하고 검증 직후 trap 으로 정리한다 (dev 클러스터 GPU 부하 상주 차단). 별도 옵션은 env 로 override 가능하다 (`GPUOBS_TIMEOUT`, `GPUOBS_POLL_INTERVAL`, `PROM_NAMESPACE`, `PROM_SVC`, `PROM_PORT`).

## 가드 단계

- 1차 (fail-on-miss): `gpuobs_mig_mode` 와 `gpuobs_mps_active` 시리즈 가 둘 다 1 이상 emit 되는지 확인. 본 단계 통과는 self-health 메트릭 wire 정합 보장.
- 2차 (fail-on-miss): `pytorch-conv2d-bench` 적용 후 `gpuobs_pod_utilization_percent{src_pod=~"pytorch-.*"}` 시리즈 1 이상 emit 대기 (timeout 기본 300s).
- 3차 (warn-only): `pod:gpu_util_p95:5m` recording rule 산출 확인. 5분 윈도우 라 bench 가동 시간 부족 시 warn 처리.

## 한계

- MIG 활성 환경 검증은 별도 environment matrix (A100/H100 노드 확보) 가 필요해 본 스크립트 범위 밖.
- MPS active 환경 검증도 dev cluster 가 MPS daemon 미실행 이라 가드 범위 밖. mps_active=0 시리즈 emit 만 확인.
