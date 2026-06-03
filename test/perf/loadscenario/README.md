# loadscenario e2e 검증 시나리오

이슈 #102 의 LoadScenario CRD 와 controller 와 spike alert 자동 검증 흐름 이 dev cluster 에서 정상 동작 하는지 회귀 가드 한다. dev cluster 전용 이며 prod 에서는 실행 하지 않는다.

## 사전 조건

- workload-injector controller mode image (`workload-injector:<VERSION>`) 가 dev cluster 에 push / load 되어 있는 상태
- `make deploy-injector-dev` 적용 으로 CRD `loadscenarios.injector.netobs.io` 와 `workload-injector-controller` Deployment 와 PodMonitor 가 cluster 에 반영 된 상태
- `correlation-stress/victim` Pod 가 dev cluster 에 상주 중. 본 가드 가 target 으로 사용
- dev cluster 의 어느 Node 라도 `environment=dev` 라벨 보유 (controller 의 cluster label gate 통과 조건)

## 시나리오 개요

- 1차 가드: CRD `loadscenarios.injector.netobs.io` 등록 확인
- 2차 가드: `workload-injector-controller` Deployment Ready 확인 (`kubectl rollout status` timeout 5 분)
- 3차 가드: 짧은 schedule (`@every 1m`) 의 LoadScenario CR 적용 후 `status.lastScheduleTime` 갱신 까지 polling 대기 (timeout 5 분)
- 4차 가드: `status.lastSuccessfulRunTime` 갱신 확인 (duration `30s` + warmup, timeout 5 분)
- spike alert 자동 검증 은 본 시나리오 의 LoadScenario 가 `spec.spikeAlertAssertion=false` 채택 으로 skip. dev cluster baseline stddev 의 환경 의존성 과 polling window (5 분) 가 verify timeout 과 정합 하지 않아 본 가드 에서 분리. spike alert 자동 검증 단독 시나리오 는 별도 follow-up 으로 위임

## 실행

```sh
./verify.sh
```

종료 직후 `trap cleanup` 으로 LoadScenario CR 과 stress Pod 가 즉시 정리 된다. dev cluster 자동 부하 상주 차단.

## 종료 코드

- 0: 1-4차 가드 통과
- 1: CRD 미등록, controller Deployment ready 실패, target Pod 부재, 또는 5 분 polling 안에 status 갱신 실패

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `LS_NAMESPACE` | `ebpf-project` | LoadScenario CR 생성 namespace |
| `LS_TARGET_NAMESPACE` | `correlation-stress` | 부하 대상 Pod namespace |
| `LS_TARGET_POD` | `victim` | 부하 대상 Pod 이름 |
| `LS_NAME` | `verify-cpu-smoke` | LoadScenario CR 이름 |
| `LS_ROLLOUT_TIMEOUT` | `300s` | controller Deployment rollout timeout |
| `LS_TIMEOUT` | `300` | status polling timeout (초) |
| `LS_POLL_INTERVAL` | `15` | status polling 간격 (초) |

## 실패 시 진단

- `[fatal] CRD ... 가 cluster 에 등록 되어 있지 않다` 로 떨어지면 `make deploy-injector-dev` 적용 여부 점검
- `[fatal] controller Deployment rollout 이 ... ready 가 되지 않았다` 로 떨어지면 controller pod 상태 (`kubectl get pod -n ebpf-project -l app.kubernetes.io/component=controller`) 와 image pull / startup log 점검
- `[fatal] target Pod ... 가 부재` 로 떨어지면 `correlation-stress` workload (`test/correlation-stress/`) 적용 여부 점검
- `[fail] ... status.lastScheduleTime 이 갱신 되지 않았다` 로 떨어지면 controller log (`kubectl logs -n ebpf-project deployment/workload-injector-controller`) 의 reconcile error 확인. cluster label gate (`environment=dev` Node 부재), schedule parse error, lock conflict 가능성
- `[fail] ... status.lastSuccessfulRunTime 이 갱신 되지 않았다` 로 떨어지면 safety gate (CheckDuration / CheckIntensity / AcquireLock) 또는 loadgen 의 stress Pod 생성 실패 가능성. controller log 와 stress Pod (`kubectl get pod -n ebpf-project -l loadscenario.name=verify-cpu-smoke`) 상태 점검
- `[warn] spike alert 미발화` 는 z-score 임계 미달 또는 baseline stddev 가 이미 큰 환경. intensity 상향 또는 다른 합성 부하 정리 후 재시도

## GPU 부하 워크로드 정책

본 가드 는 `cpu` kind 만 사용 (target `correlation-stress/victim` 이 GPU 미부착). gpu kind 의 자동 부하 검증 은 별도 시나리오 로 분리 가능 하며 본 가드 의 hard 조건 에서 제외. cpu/memory/network kind 의 stress Pod 는 LoadScenario 삭제 시 finalizer 가 가비지 수거 한다.
