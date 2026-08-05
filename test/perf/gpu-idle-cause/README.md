# gpu-idle-cause e2e 검증 시나리오

이슈 #101의 victim 단위 GPU idle cause weight 보강이 dev cluster에서 정상 emit되는지 회귀 가드한다. dev cluster 전용이며 prod에서는 실행하지 않는다.

## 사전 조건

- `gpuobs-agent` 가 gpu 노드에서 `Running` 상태이며 `node:gpu_idle:5m` 가 emit 중인 상태
- `make deploy-gpuobs-dev` 와 `make deploy-dashboards` 가 적용 되어 PrometheusRule `netobs-gpuobs-correlation` 의 #101 신규 rule (recording 7 종 과 alert 2 종) 과 `gpu-network-correlation-dashboard` ConfigMap 의 panel 2 종 (id 2 와 3) 이 cluster 에 반영 된 상태
- 권장: `correlation-stress` namespace 의 합성 부하 (victim 과 suspect-sync 와 suspect-async) 가 상주 중. ambiguous alert 자연 firing 신호를 얻을 수 있다. 합성 부하 없이 idle dev cluster 에서는 1차 가드만 통과하고 ambiguous alert 는 warn 으로 처리

## 시나리오 개요

- 1차 가드: PrometheusRule 의 recording rule 7 종 과 alert rule 2 종 이 적용 되어 있는지 직접 inspect
- 2차 가드: `gpu-network-correlation-dashboard` ConfigMap 안에 panel id 2 와 3 의 신규 표현식 (`pod:gpu_idle_cause_weight:5m` 와 `victim:gpu_idle_dominant_cause_indicator:5m`) 이 포함 되어 있는지 inspect
- 3차 가드: 5 분 warmup 후 신규 recording rule 7 종 이 모두 1 개 이상 시리즈 emit 되는지 prometheus query
- 4차 가드 (warn only): `GPUIdleDominantCauseAmbiguous` 또는 `VictimGPUIdleDominantCauseAmbiguous` alert 가 pending 또는 firing 상태인지 확인. idle dev cluster 에서는 비어 있을 수 있어 warn 으로만 처리

## 실행

```sh
./verify.sh
```

## 종료 코드

- 0: 1차 와 2차 와 3차 가드 통과 (4차 가드는 warn 처리, 종료 코드 영향 없음)
- 1: PrometheusRule 또는 dashboard ConfigMap 누락, 또는 5 분 warmup 안에 recording rule 시리즈 emit 실패

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `GPUIDLE_NAMESPACE` | `ebpf-project` | PrometheusRule 과 dashboard ConfigMap 이 위치 한 namespace |
| `GPUIDLE_TIMEOUT` | `600` | recording rule 시리즈 emit 대기 timeout (초). 5 분 warmup 고려 600 |
| `GPUIDLE_POLL_INTERVAL` | `30` | prometheus query polling 간격 (초) |
| `GPUIDLE_PROM_NAMESPACE` | `monitoring` | prometheus service 가 위치 한 namespace |
| `GPUIDLE_PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus service 이름 |
| `GPUIDLE_PROM_PORT` | `9090` | prometheus service port |
| `GPUIDLE_PROM_IP` | (auto) | prometheus ClusterIP override |

## 실패 시 진단

- `[fatal] PrometheusRule netobs-gpuobs-correlation 가 ${NAMESPACE} 에 존재 하지 않는다` 로 떨어지면 `make deploy-gpuobs-dev` 적용 여부 점검
- `[fatal] recording rule '<name>' 가 PrometheusRule 에 누락` 로 떨어지면 `deploy/gpuobs/base/prometheus-rule-idle-cause.yaml` 의 `netobs-gpuobs.dominant-cause.recording` group 안에 해당 record 가 있는지 확인 후 재배포
- `[fatal] alert rule '<name>' 가 PrometheusRule 에 누락` 로 떨어지면 `netobs-gpuobs.gpu-idle.alerts` group 안에 해당 alert 정의 확인
- `[fatal] gpu-network-correlation-dashboard ConfigMap 이 ${NAMESPACE} 에 존재 하지 않는다` 또는 `[fatal] dashboard panel 표현식 ... 누락` 로 떨어지면 `make deploy-dashboards` 적용 여부 와 `deploy/dashboards/gpu-network-correlation.json` 의 panel 등록 확인
- `[fail] timed out waiting for victim 단위 cause weight recording rule series` 로 떨어지면 gpu node 의 `gpuobs-agent` Pod 상태 (`node:gpu_idle:5m` emit 여부) 와 cAdvisor / kube-state-metrics 정상 동작 점검. idle 게이팅 비활성 시간대 (GPU active) 에는 weight 와 dominant cause 시리즈 가 모두 emit 되지 않으므로 GPU idle 시간대 에 재실행

## GPU 부하 워크로드 정책

본 가드 는 추가 GPU 부하 워크로드 를 spawn 하지 않는다. dev cluster 의 자연 cause 신호 와 기존 `correlation-stress` namespace 합성 부하 만 으로 통과 가능 하도록 설계 되어 있다. 합성 부하 가 필요한 경우 별도 `test/perf/*.yaml` workload 를 명시 apply 한 뒤 검증 직후 즉시 `kubectl delete` 로 정리 한다 (dev 클러스터 상주 금지).
