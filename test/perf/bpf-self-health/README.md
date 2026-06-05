# bpf-self-health e2e 회귀 가드 (#105)

이슈 #105 의 BPF program attach self-health 메트릭 도입을 dev cluster 에서 검증하는 회귀 가드 스크립트다. 3 단계 가드 (정상 attach 누적 / fake symbol 실패 발화 / 시뮬 정리) 로 신규 메트릭 wire 정합 과 attach 실패 분류 정확성 을 자동 확인한다. kernel 버전 mismatch 와 BTF 부재 시뮬은 별도 environment matrix 가 필요해 본 가드 범위 밖이며 dev cluster kernel 6.8 단일 환경 의 fake symbol 시뮬로 갈음한다.

## 실행

```sh
test/perf/bpf-self-health/verify.sh
```

스크립트가 자동으로 `NETOBS_BPF_FAKE_ATTACH_SYMBOLS` env 를 daemonset 에 patch 후 rollout 하고, 검증 직후 env 정리 + rollout 으로 시뮬 종료한다 (trap cleanup 으로 비정상 종료 시도 자동 복구). env 와 timeout 은 다음으로 override 가능.

- `BPF_TIMEOUT` (기본 300s): 단계별 polling timeout
- `BPF_POLL_INTERVAL` (기본 15s): polling 간격
- `BPF_FAKE_SYMBOL` (기본 `__netobs_nonexistent_probe`): 인위적 attach 실패 대상 symbol 이름

## 가드 단계

- 1차 (fail-on-miss): `netobs_bpf_program_attach_total{program="tcp_sendmsg",result="success"} > 0` 확인. required kprobe 가 정상 attach 되어 누적되는 회귀 차단.
- 2차 (fail-on-miss): fake symbol env patch + rollout 후 `attach_total{result="failure"}` 와 `attach_retry_total{reason="symbol_not_found"}` 시리즈 1 이상 발화 확인. retry budget 소진 후 failure 마감 의미 검증.
- 3차 (cleanup): env unset + rollout 으로 시뮬 종료. fake symbol 시리즈는 stale 로 남되 정상 program 의 신호는 회복.

## 한계

- kernel 버전 mismatch (`reason=kernel_version_mismatch`) 시뮬은 dev cluster 가 kernel 6.8 단일 환경이라 별도 environment matrix (구 kernel 노드 추가) 필요.
- BTF 부재 (`reason=btf_missing`) 시뮬은 BTF-stripped 컨테이너 이미지 빌드 필요.
- verifier_rejected 시뮬은 BPF source 의 의도적 invalid 패턴 도입 필요로 본 가드 범위 밖.

위 3종 reason 의 분류 정확성은 단위 테스트 (`internal/netobs/ebpf/attach_errors_test.go`) 가 cover 한다.
