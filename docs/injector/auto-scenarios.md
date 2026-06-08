# LoadScenario 자동 부하 시나리오 운영 가이드

이슈 #102 의 LoadScenario CRD 와 reconciler 기반 자동 부하 시나리오 운영 가이드다. dev cluster 에서 schedule 따라 자동 으로 cpu/memory/network/gpu 부하를 인가 하고 z-score spike alert 발화 여부 를 자동 검증 한다. prod cluster 자동 부하 인가는 비목표 이며 cluster label gate 와 overlay 구조 양쪽 에서 차단 한다.

## 적용 대상과 비목표

- 적용 대상은 dev cluster 의 `environment=dev` 라벨 노드. 운영자가 매일 자정 또는 임의 schedule 로 합성 부하를 트리거 해 #89 의 z-score spike alert 와 #66 의 dominant cause 분석 회귀 가드 를 자동화 하는 흐름.
- 비목표 는 prod cluster 자동 부하 (controller 자체 가 dev overlay 한정 배포), 실시간 부하 조정 (dynamic intensity tuning 은 별도 follow-up), multi controller HA replicas (leader election 으로 단일 active 만 허용), LoadScenario dependency graph (다른 LoadScenario 완료 후 trigger), admission webhook 기반 차단 (cluster label gate 만 으로 충분 하다는 판단).

## CRD spec 필드

| 필드 | 타입 | 기본 | 설명 |
|---|---|---|---|
| `spec.schedule` | string | 필수 | cron 5 필드 (`0 0 * * *` 매일 자정) 또는 descriptor (`@every 1m`, `@daily`) |
| `spec.kind` | enum | 필수 | `cpu` 와 `memory` 와 `network` 와 `gpu` 중 택일 |
| `spec.duration` | metav1.Duration | 필수 | 1 회 부하 인가 시간 (`30s`, `5m`, max `30m`) |
| `spec.intensity` | string | 필수 | kind 별 단위 (cpu: `500m`, memory: `512Mi`, network: `100M`, gpu: `1`) |
| `spec.targetRef.namespace` | string | 필수 | 부하 대상 Pod namespace |
| `spec.targetRef.name` | string | 필수 | 부하 대상 Pod 이름 |
| `spec.concurrencyPolicy` | enum | `Forbid` | `Allow` 와 `Forbid` 와 `Replace` |
| `spec.spikeAlertAssertion` | bool | `false` | 부하 종료 후 spike alert 자동 검증 활성화 |
| `spec.maxFailures` | int | `3` | 연속 실패 임계, 초과 시 자동 suspend |
| `spec.suspend` | bool | `false` | 명시 disable 또는 자동 suspend 상태 |

## status 필드

| 필드 | 의미 |
|---|---|
| `status.lastScheduleTime` | controller 가 마지막 으로 run 을 트리거 한 시각 |
| `status.lastSuccessfulRunTime` | 마지막 정상 종료 run 의 시각 |
| `status.lastObservedSpikeAlerts` | spike alert 자동 검증 hit 한 alertname 목록 |
| `status.consecutiveFailures` | 마지막 successful run 이후 연속 실패 횟수 |
| `status.conditions` | `Ready` 와 `Scheduled` 와 `SpikeAlertObserved` 와 `Suspended` 4 종 condition |

## concurrencyPolicy 운영적 의미

- `Forbid` 는 진행 중 injection 의 lock 이 해제 될 때까지 다음 run 을 skip 한다. 가장 안전한 기본값. lock 충돌 은 `Scheduled` condition 의 `ForbidLockHeld` reason 으로 status 에 기록 된다.
- `Replace` 는 진행 중 injection 의 lock 을 강제 해제 한 뒤 새 run 을 트리거 한다. 같은 target 의 이전 부하 가 진행 중 이라도 즉시 새 부하 로 전환. 운영 의도 가 명확 할 때 만 사용.
- `Allow` 는 다른 target 의 진행 중 injection 과 병행 가능 하지만 동일 target 의 동시 Allow 는 AcquireLock 자체 가 차단 한다. 다수 LoadScenario 가 서로 다른 target 을 동시 부하 할 때 유용.

## spikeAlertAssertion 자동 검증 흐름

`spec.spikeAlertAssertion = true` 이면 controller 가 부하 종료 후 5 분 polling window 동안 prometheus 의 ALERTS 시리즈 를 30 초 간격 으로 query 한다. 다음 4 종 alert 중 1 종 이라도 `firing` 상태 면 hit 로 기록 된다.

- `GPUUtilSpikeDetected`
- `NetworkDropSpikeDetected`
- `CPUThrottleSpikeDetected`
- `MemoryPressureSpikeDetected`

hit 한 alertname 목록 은 `status.lastObservedSpikeAlerts` 에 기록 되고 `condition.SpikeAlertObserved` 가 `True` 로 전환 된다. window 만료 까지 hit 가 없으면 `condition.SpikeAlertObserved = False` 로 기록 된다. 본 흐름 은 #89 의 z-score spike alert 와 #102 의 자동 부하 인가 통합 회귀 가드 역할 을 한다.

## prod cluster 차단 메커니즘

prod cluster 자동 부하 인가 를 차단 하기 위해 2 layer 가드 가 적용 된다.

- Layer 1 (코드): controller 시작 시 와 reconcile 매 호출 시 `CheckClusterLabel(environment=dev)` 호출. dev cluster 가 아니면 cluster 의 어떤 Node 도 `environment=dev` 라벨 을 갖지 않아 fail-fast 처리 되며 reconcile 이 즉시 종료 한다.
- Layer 2 (배포): injector overlay 자체 가 dev only (`deploy/injector/overlays/dev/` 만 존재, prod 부재). prod cluster 에 운영자 가 의도적 으로 적용 할 수 없는 구조.

## CRD 첫 배포 절차

```sh
# 1. CRD 적용 (controller 가 아직 없어도 CRD 자체는 등록 가능)
kubectl apply -f deploy/injector/base/injector.netobs.io_loadscenarios.yaml

# 2. controller deployment 와 RBAC 와 PodMonitor 적용
make deploy-injector-dev

# 3. controller pod ready 확인
kubectl rollout status -n ebpf-project deployment/workload-injector-controller

# 4. LoadScenario CR 적용 (사용자 정의)
kubectl apply -f my-loadscenario.yaml
```

## 샘플 LoadScenario CR

매분 1 회 `correlation-stress/victim` Pod 에 30 초 CPU 부하 인가 + spike alert 자동 검증:

```yaml
apiVersion: injector.netobs.io/v1alpha1
kind: LoadScenario
metadata:
  name: dev-cpu-smoke
  namespace: ebpf-project
spec:
  schedule: "@every 1m"
  kind: cpu
  duration: 30s
  intensity: 500m
  targetRef:
    namespace: correlation-stress
    name: victim
  concurrencyPolicy: Forbid
  spikeAlertAssertion: true
  maxFailures: 3
```

## examples 디렉토리 활용 (#118)

위 inline 샘플은 빠른 참조용이고 `kubectl apply` 로 직접 사용 가능한 base manifest 8종은 `deploy/injector/examples/` 에 별도 정리되어 있다 (#118). 운영자가 신규 LoadScenario CR을 작성할 때 본 디렉토리의 yaml을 복사 후 `targetRef`와 `schedule`만 운영 환경에 맞춰 정정하면 즉시 적용 가능하다.

### 추천 base 매트릭스

| 시나리오 | 추천 base yaml |
|---|---|
| CPU 부하 (cgroup throttling 자극) | `cpu-stress-scenario.yaml` |
| memory 압박 (OOM 인접 신호) | `memory-pressure-scenario.yaml` |
| GPU 부하 (RTX 3090 단일 GPU) | `gpu-load-scenario.yaml` |
| network 트래픽 (iperf3 multi-stream) | `network-load-scenario.yaml` |
| 동일 target 단일 lock 시나리오 | `concurrency-allow-scenario.yaml` |
| 기존 run 보유 시 skip 의도 | `concurrency-forbid-scenario.yaml` |
| 기존 lease 해제 후 신규 trigger | `concurrency-replace-scenario.yaml` |
| 부하 종료 후 z-score spike alert 자동 검증 | `spike-assertion-scenario.yaml` |

### 운영자 워크플로우

- `deploy/injector/examples/` 의 추천 base yaml을 운영 환경의 임시 디렉토리로 복사
- `targetRef.namespace`와 `targetRef.name`을 실제 부하 인가 대상 Pod로 정정
- `schedule` 의 cron 표현식을 운영 의도에 맞춰 조정 (`@every 1m` 같은 짧은 schedule은 dev cluster 검증 한정)
- `intensity` 의 안전 상한 (cpu 4000m, memory 2Gi, network 1000M, gpu 1) 을 넘지 않게 정정
- `kubectl apply -f <정정한-yaml>` 적용 후 `kubectl get loadscenario -n <namespace>` 로 controller reconcile 진입 확인
- `status.lastScheduleTime`과 `status.lastSuccessfulRunTime` 갱신으로 정상 동작 확인

## Troubleshooting

### reconciler stalled (`LoadScenarioReconcilerStalled` critical alert)

`time() - max(loadscenario_reconcile_timestamp_seconds) > 300` 가 1 분 지속 되면 발화. 원인 가능성.

- controller worker 가 blocking 부하 인가 (`spec.duration`) 안에서 stuck. controller pod log 확인 후 worker thread 상태 점검
- leader election lease 분실. `kubectl get lease -n ebpf-project loadscenario.injector.netobs.io` 의 holder 와 controller pod 의 hostname 일치 여부 확인
- manager goroutine 죽음. `kubectl logs -n ebpf-project deployment/workload-injector-controller --previous` 로 직전 crash log 확인

### lock conflict (`condition.Scheduled = False` with `ForbidLockHeld` reason)

`concurrencyPolicy = Forbid` 인 LoadScenario 가 다음 run 을 트리거 하려 했을 때 다른 진행 중 injection 의 lock 이 잡혀 있는 상태. ConfigMap `workload-injector-lock-<targetNamespace>-<targetPod>` (safety.LockName 컨벤션) 의 `injector.lease/expires` annotation 으로 만료 시각 확인. 정상 lifecycle 에서는 lease TTL (duration*2) 만료 시 자연 해제 된다.

### spike alert 미발화 (`condition.SpikeAlertObserved = False`)

부하 가 z-score spike alert 임계 (#89 의 stddev 기반) 를 넘기지 못 한 상태. 가능성.

- `spec.intensity` 가 baseline 변동 보다 작음. intensity 를 더 높여 재시도 (단 safety.CheckIntensity 상한 내)
- `spec.duration` 이 너무 짧아 (예: 30 초 미만) baseline 갱신 전 부하 종료. duration 을 1 분 이상 으로 조정
- z-score baseline 의 stddev 가 이미 큰 환경 (이미 다른 합성 부하 가 상주 중). 다른 부하 정리 후 재시도

### consecutive failures (`LoadScenarioConsecutiveFailures` warning)

`status.consecutiveFailures` 가 `spec.maxFailures` 초과 시 `spec.suspend` 가 자동 `true` 로 전환 되어 다음 run 트리거 가 멈춘다. 운영자 검토 후 `kubectl patch loadscenario/<name> -n ebpf-project --type=merge -p '{"spec":{"suspend":false}}'` 로 재개.
