# drop-stack e2e 검증 시나리오

이슈 #83 의 drop event kernel stack trace capture 가 dev cluster 에서 정상 emit 되는지 회귀 가드 한다. `test/perf/rca-e2e` 와 동일 패턴으로 dev cluster 전용이며 prod 에서는 실행 하지 않는다.

## 사전 조건

- dev cluster 의 `netobs-agent` DaemonSet 가 #83 PR 의 이미지로 업데이트 되어 있음
- `netobs-agent` 컨테이너의 env `NETOBS_DROP_STACK_ALLOW_NAMESPACES` 가 트리거 namespace (기본 `observability-test`) 를 포함 함. 미설정 시 stack 메트릭 emit 자체가 skip 되어 본 시나리오가 timeout 으로 떨어진다
- `kube-prometheus-stack-prometheus` Service 가 monitoring namespace 에 ready 상태
- 본 스크립트 실행 호스트가 dev cluster 의 Service CIDR 에 routable

## 두 trigger 패턴

| 모드 | 메커니즘 | 적용 조건 |
|---|---|---|
| `nc-noport` (기본) | 비-listening 포트 로의 TCP 연결 시도 → kernel 이 TCP_CLOSE reason 으로 drop | 모든 dev 노드 |
| `cnp-drop` | cilium CNP 의 egress 8888/tcp 차단 + 동일 포트로 nc 시도 | cilium 의 datapath drop 이 `kfree_skb_reason` 을 호출 하는 cilium 버전 |

`DROP_STACK_TRIGGER_MODE=cnp-drop` env 로 override 가능 하다. dev cluster 의 자연 drop 6.79/s (`NOT_SPECIFIED`, `QUEUE_PURGE`, `REASON_1`, `TC_EGRESS`, `QDISC_DROP`) 가 동시에 측정 되므로 두 trigger 중 하나만 동작 해도 메트릭 emit 이 확인 가능 하다.

## 실행

```sh
./verify.sh
```

본 스크립트 는 trigger 적용 후 최대 300 초 (15 초 간격 polling) 안 에 `netobs_drop_stack_total` 시리즈가 1 개 이상 노출 되는지 확인 한다. 임계값 은 `DROP_STACK_TIMEOUT` env 로 override 가능 하다. 마지막에 는 메모리 규칙 에 따라 두 trigger manifest 와 임시 CNP 를 모두 정리 한다.

## 종료 코드

- 0: 검증 통과 (`netobs_drop_stack_total` 시리즈 1 개 이상 노출)
- 1: 검증 실패 (시리즈 timeout, prometheus 미접근, allow-list 누락 의심 등)

## 환경 변수

| 변수 | 기본값 | 설명 |
|---|---|---|
| `DROP_STACK_NAMESPACE` | `observability-test` | trigger Pod 가 동작 하는 namespace |
| `DROP_STACK_TRIGGER_MODE` | `auto` (실제로는 `nc-noport`) | trigger 패턴 선택. `nc-noport` / `cnp-drop` |
| `DROP_STACK_TIMEOUT` | `300` | 시리즈 emit 대기 timeout 초 |
| `DROP_STACK_POLL_INTERVAL` | `15` | prometheus polling 주기 초 |
| `DROP_STACK_PROM_NAMESPACE` | `monitoring` | prometheus Service namespace |
| `DROP_STACK_PROM_SVC` | `kube-prometheus-stack-prometheus` | prometheus Service 이름 |
| `DROP_STACK_PROM_PORT` | `9090` | prometheus Service port |
| `DROP_STACK_PROM_IP` | (자동 lookup) | prometheus ClusterIP override |

## 실패 시 진단

`[fail] timed out` 으로 떨어지면 다음 순서로 확인 한다.

- `netobs-agent` 의 env 에 `NETOBS_DROP_STACK_ALLOW_NAMESPACES=observability-test` 가 포함 되어 있는지
- `netobs_drop_stack_resolver_cache_hits_total` 또는 `netobs_drop_stack_resolver_cache_misses_total` 가 0 보다 큰지 (0 이면 resolver init 실패)
- `netobs-agent` 의 stdout 에 `drop stack resolver: enabled` 가 출력 되었는지 (그렇지 않으면 `/proc/kallsyms` 접근 실패 의심)
- `netobs_bpf_program_loaded{symbol="kfree_skb_reason"}` 가 1 인지 (그렇지 않으면 kprobe attach 실패)
