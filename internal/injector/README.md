# internal/injector

workload-injector 가 사용하는 라이브러리 모음이다. injector binary 자체는 `cmd/workload-injector` 에 있으며 본 디렉토리는 (1) 부하 모듈 (loadgen), (2) blast radius 산출 (blastradius), (3) 안전 가드 (safety), (4) Prometheus exporter (exporter) 네 패키지를 둔다.

## 책임 분리

- `loadgen/` — kind 별 부하 모듈 (`cpu`, `memory`, `network`, `gpu`) 의 공통 인터페이스와 K8s API 로 stress Pod 를 spawn / cleanup 하는 helper. `gpu` 는 CUDA devel 이미지에서 nvcc 로 즉석 컴파일한 busy kernel 을 duty cycle 로 돌려 실제 GPU 점유 를 발생시킨다
- `blastradius/` — baseline 과 impact 두 시계열의 평균 latency 차이를 0 ~ 1 정규화한 score 산출과 victim 후보 식별
- `safety/` — duration / intensity 상한, cluster Node 라벨 gate, target 별 ConfigMap annotation lease 로 동시 injection 차단
- `exporter/` — `injector_active`, `correlation_blast_radius_score`, baseline / impact, self-health 메트릭의 `prometheus.Collector` 구현체

## 동작 lifecycle

`cmd/workload-injector` 는 다음 7 단계로 한 cycle 의 injection 을 수행한다.

1. 입력 검증과 안전 가드 (`safety.CheckDuration`, `safety.CheckIntensity`, `safety.CheckClusterLabel`, target Pod 존재 검증, `safety.AcquireLock`)
2. baseline fetch (`correlation.NewPrometheusFetcher` 로 `BASELINE_WINDOW` 동안의 victim latency p99 수집)
3. 부하 시작 (`loadgen.Start`) 과 `injector_active.Set(1)`
4. `DURATION` 만큼 대기 (ctx cancel 시 즉시 cleanup 흐름)
5. `injector_active.Set(0)` 으로 transition 후 `loadgen.Stop` 으로 spawn 한 Pod cleanup
6. impact fetch (부하 종료 직후 `BASELINE_WINDOW` 동안 latency 수집) 와 `blastradius.Compute` 으로 victim 별 score 산출
7. `Collector.ReplaceBlast` 후 `linger` 30 초 동안 active=0 메트릭을 유지하다 `ClearActive` 로 시계열 emit 중단

## 노출 메트릭

- `injector_active{target_namespace, target_pod, target_node, kind}` gauge: 1 (활성) / 0 (비활성). 부하 종료 직후 30 초 linger 동안 0 으로 유지되어 PodMonitor 의 마지막 scrape 가 transition 을 정확히 잡는다.
- `correlation_blast_radius_score{target_namespace, target_pod, target_node, victim_namespace, victim_pod, victim_pod_uid, kind}` gauge: 0 ~ 1 정규화 score. `(impact - baseline) / baseline` 을 clamp.
- `injector_baseline_latency_seconds{...동일 라벨...}` gauge: 산출 입력 디버깅용
- `injector_impact_latency_seconds{...동일 라벨...}` gauge: 산출 입력 디버깅용
- `injector_duration_seconds{kind}` gauge: 마지막 cycle 의 부하 walltime
- `injector_runs_total{kind, status}` counter: status ∈ {`ok`, `error`, `skipped_gate`}
- `injector_errors_total{kind, stage}` counter: stage ∈ {`baseline_fetch`, `loadgen_start`, `loadgen_stop`, `impact_fetch`, `cleanup`}

## 안전 가드

본 이슈의 비목표 "prod 자동 실행 금지" 를 binary 단에서 명시 검증한다.

- `DURATION` 절대 상한 30 분. 초과 시 fail-fast
- `INTENSITY` kind 별 상한: cpu 4000m, memory 2Gi, network 1000M, gpu 100 (목표 점유율 percent)
- `INJECTOR_ALLOW_CLUSTER_LABEL` (기본 `environment=dev`) 매칭 Node 가 cluster 에 0 개이면 fail-fast. prod cluster 는 `environment=prod` 만 가지도록 라벨링되어 있어야 효과가 있다 (운영자 컨벤션)
- 동일 target 동시 injection 차단: ConfigMap annotation lease 패턴으로 lock 획득 실패 시 fail-fast. lease TTL 은 duration 의 2 배

가드 실패는 모두 `injector_runs_total{status="skipped_gate"}` 누적 후 `os.Exit(1)`.

## alert runbook

### InjectorActive

severity `info`. workload-injector 가 부하 발사 중이라는 사실 자체를 운영자에게 알린다. 부하 종료 후 active=0 reset 으로 자동 해소된다.

- alert label 의 `target_namespace/target_pod`, `kind` 를 확인
- Grafana `workload-injector` 대시보드의 timeline panel 에서 부하 시작 시점 확인
- 부하가 의도된 합성 검증인지 확인. 의도하지 않은 injection 이면 cluster Node 라벨 또는 RBAC 가 잘못된 상태

### BlastRadiusHigh

severity `warning`. 합성 부하가 victim Pod 의 latency 를 baseline 대비 50 % 이상 증가시킨 경우다.

- alert label 의 `victim_namespace/victim_pod` 를 확인
- 대시보드의 Blast radius 테이블에서 score 가 0.85 이상이면 부하 강도가 운영 워크로드에 명백한 영향. 즉시 부하 중단 (Job 삭제) 검토
- score 가 0.5 ~ 0.7 이면 의도된 검증 범위 내. 운영 워크로드에 미치는 영향이 합리적인지 운영자가 판단

### InjectorStuck

severity `critical`. binary 의 `DurationLimit` (30 분) 을 넘어 active=1 시계열이 35 분 지속된 상태. 정상 lifecycle 에서는 절대 발생하지 않는다.

- `kubectl -n ebpf-project get pods -l app.kubernetes.io/name=workload-injector` 로 Job Pod 상태 확인. 정상 종료 못한 Pod 가 있으면 강제 삭제
- spawn 된 stress Pod 의 잔존 여부 확인: `kubectl get pods -A -l app.kubernetes.io/name=workload-injector,app.kubernetes.io/component=stress`. 잔존 시 수동 cleanup
- ConfigMap lease 잔존: `kubectl -n ebpf-project get configmap -l app.kubernetes.io/component=lease`. 만료된 lease 는 운영자가 직접 삭제 (binary 가 만료된 lease 를 다음 cycle 에 자동 갱신하지만 수동 정리로 즉시 해소)

## 운영자 사용 절차

```sh
# 1) build / image
make build-workload-injector
make image-build-workload-injector

# 2) base 매니페스트 적용 (1 회)
make deploy-injector-dev

# 3) 단일 injection 실행. test/injector-examples/ 의 cpu / network / gpu 3 종 예제 Job manifest 를
#    직접 edit (TARGET_POD / INTENSITY / DURATION) 후 apply 한다. kubectl create job --from 패턴은
#    base job 의 env 가 placeholder 라 부적합하다.
vim test/injector-examples/cpu.yaml
kubectl apply -f test/injector-examples/cpu.yaml

# 4) 메트릭 관찰
PROM_POD=$(kubectl get pod -n monitoring -l app.kubernetes.io/name=prometheus -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n monitoring $PROM_POD -c prometheus -- \
  wget -qO- 'http://localhost:9090/api/v1/query?query=injector_active' | jq

# 5) Job 정리 (ttlSecondsAfterFinished 120s 후 자동 삭제되지만 수동 정리 가능)
kubectl delete -f test/injector-examples/cpu.yaml
```

## 카디널리티 분석

`injector_active` 는 target 별 단일 시계열, `correlation_blast_radius_score` 는 `MAX_VICTIMS` (기본 20) 으로 cap 되어 한 cycle 의 series 총 수는 `1 + 20 * 4` (baseline / impact / score) ≒ 81 이다. Job lifecycle 이 종료되면 시계열 자체가 사라져 누적되지 않는다.

## 비목표

- prod cluster 자동 실행은 binary 단의 cluster label gate 로 차단된다. follow-up 으로 자동 실행 워크플로 (예: scheduled CronJob with approval gate) 가 분리된다.
- 합성 부하의 정확한 latency 영향 모사 (실제 운영 워크로드의 트래픽 패턴, gRPC / HTTP2 등) 는 본 이슈 범위 밖이다. 첫 구현은 cpu throttle / iperf3 / cuda busy loop 의 단순 신호로 시작한다.
