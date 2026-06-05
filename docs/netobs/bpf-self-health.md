# BPF program attach self-health 운영 가이드 (#105)

본 문서는 netobs-agent 가 #105 에서 도입한 BPF program attach 가시화 메트릭 3종 (`netobs_bpf_program_loaded`, `netobs_bpf_program_attach_total`, `netobs_bpf_program_attach_retry_total`) 의 운영적 의미와 7종 reason enum 별 대응 절차, 그리고 troubleshooting 흐름을 정리한다. 본 PR 이전에는 attach 실패 시 운영자가 metric 부재만으로 진단해야 했던 actionable 가시성 부재 결함을 해소한다.

## 메트릭 의미 분리 매트릭스

세 메트릭은 동일한 attach 도메인을 다른 시간 축으로 노출한다.

- `netobs_bpf_program_loaded{symbol}` (gauge, 0/1): 현재 attach 상태의 즉시 스냅샷. 1=attached, 0=detached. agent 재시작 시 startup 단계에서 0 으로 사전 등록되고 attach 성공 시 1 로 올라간다. retry budget 소진 후에도 0 그대로 남는 program 은 영구 실패.
- `netobs_bpf_program_attach_total{program, result}` (counter): startup 이후 누적 시도 카운터. result=success 는 retry budget 안에서 결과적으로 성공한 attach 시도, result=failure 는 budget 소진 후 마감된 시도. counter 라 agent restart 시 0 으로 리셋 되며 본 동작은 "본 에이전트 인스턴스 의 attach 시도 빈도" 운영 의미와 자연 정합 한다.
- `netobs_bpf_program_attach_retry_total{program, reason}` (counter): retry 부담 누적 카운터. attach 호출 1 회의 매 retry 시도 마다 7종 reason enum 라벨로 +1 누적. result=success 이지만 retry_total 이 0 이 아닌 program 은 transient flap 으로 식별 가능 하다.

운영 시 세 메트릭의 조합 의미는 다음과 같다.

- `loaded=1` + `retry_total=0`: 정상 환경. attach 첫 시도 성공.
- `loaded=1` + `retry_total>0`: transient flap. retry 후 결과적으로 성공. root cause 추적 필요 (kernel race / capability 부여 지연 등).
- `loaded=0` + `attach_total{result="failure"}>0`: 영구 실패. retry budget 소진 후에도 attach 안 됨. 즉시 대응 필요.
- `loaded=0` + `attach_total=0`: attach 단계 미진입. LoadNetObsObjects 실패 또는 capability 자체 부재. `up{job="netobs-agent"}` 와 agent log 점검.

## retry 정책 근거

retry 정책은 linear backoff `500ms`, max retries `3`회, 전체 budget `5s` 로 hardcoded.

- `500ms` backoff 근거는 CO-RE relocation 의 driver init 비용 추정 으로 kernel BTF resolve 와 verifier 부담이 통상 200-400ms 라 한 사이클 안에 끝나는 시간 단위로 설정.
- `3`회 max retries 는 transient flap 흡수 (대부분 1-2회 안에 회복) 와 영구 실패 빠른 식별 (5초 안에 fail-close) 의 균형점.
- `5s` total budget 은 agent startup 흐름의 SLI 측면에서 readiness probe 의 initialDelaySeconds (10s) 안에 마감 보장.

운영 중 dynamic policy tuning (운영자가 운영 중 backoff / max retries 조정) 은 본 PR 비목표.

## 7종 reason enum 의 운영적 해석

`netobs_bpf_program_attach_retry_total` 의 `reason` 라벨은 `internal/netobs/ebpf/attach_errors.go` 의 `AttachReason` enum 7종 으로 폐쇄 유지.

- `symbol_not_found`: kernel symbol lookup 실패 (kprobe attach 대상 함수 부재). kernel 버전 변경 으로 inline 화 / 이름 변경 / 제거 된 경우. 대응: `cat /proc/kallsyms | grep <symbol>` 로 실재 여부 확인 후 kernel 버전별 alternative symbol 검토.
- `kernel_version_mismatch`: kernel 자체가 본 BPF feature (BTF / 특정 helper / program type) 를 미지원. cilium/ebpf 의 `ErrNotSupported` wrapping 흡수. 대응: 노드 kernel 버전 확인 (`uname -r`) 후 최소 요구 버전 (6.x) 대비.
- `btf_missing`: BTF 정보 부재. `/sys/kernel/btf/vmlinux` 부재 또는 컨테이너 mount 누락. 대응: DaemonSet hostPath mount 확인 후 호스트의 `CONFIG_DEBUG_INFO_BTF=y` 빌드 검증.
- `verifier_rejected`: BPF verifier 가 program 을 거부. loop / pointer arithmetic / map access 의 invalid 패턴 또는 kernel verifier flag 변경. 대응: agent log 의 verifier 출력 확인 후 BPF source 수정.
- `permission_denied`: CAP_BPF / CAP_PERFMON / CAP_SYS_RESOURCE / CAP_SYS_PTRACE 부족. 대응: DaemonSet securityContext.capabilities.add 확인 후 누락 capability 부여.
- `link_internal_error`: cilium/ebpf 의 link 패키지 내부 오류. perf_event_open / tracefs 등 의 부수 오류. 대응: agent log 의 정확한 에러 메시지 확인 후 cilium/ebpf 버전 호환성 검토.
- `other`: 위 분기 미매핑. 신규 에러 매핑이 추가 될 때까지의 임시 슬롯. 운영자가 발견 시 `internal/netobs/ebpf/attach_errors.go` 의 `classifyAttachError` 에 새 매핑 추가 필요.

## alert 의미 분리

본 PR 은 self-health alert 2종이 독립 신호를 cover 한다.

- `NetObsBpfProgramUnavailable` (기존, `netobs_bpf_program_loaded == 0` for `5m`): 현재 attach 미상태의 영구 실패 신호. 5분 이상 attach 안 된 program 발화. agent crash 의 ObsAgentDown critical 과 다른 단계.
- `NetObsBpfAttachFailureHigh` (신규 #105, `sum by(program) (rate(netobs_bpf_program_attach_total{result="failure"}[5m])) > 0.1` for `2m`): 시도 빈도 spike 신호. retry budget 안에서 transient flap 이 반복되면 영구 실패 전 단계에서 먼저 발화 해 root cause 진단 시간 확보.

## Troubleshooting

- 영구 실패 식별: `netobs_bpf_program_loaded{symbol="X"} == 0` 가 5분 지속되고 `netobs_bpf_program_attach_total{program="X",result="failure"} > 0` 이 함께 hit 되면 영구 실패. `attach_retry_total` 의 reason 라벨로 분기.
- transient flap 해석: `netobs_bpf_program_loaded{symbol="X"} == 1` 이지만 `netobs_bpf_program_attach_retry_total{program="X"} > 0` 이면 retry 후 성공한 flap. root cause 추적 필요 하지만 즉시 대응 우선순위 는 낮음.
- retry budget 소진 후 next step: required program 의 경우 agent 가 fail-close (crash). optional program 의 경우 agent 는 계속 동작 하지만 해당 receive-path / drop-path 메트릭이 부재. 운영자는 본 메트릭 fan-out 가시화 로 어느 워크로드 신호 가 빠졌는지 식별.
- required vs optional 분기: `tcp_sendmsg` / `tcp_sendmsg_ret` 2 종 만 required 이고 나머지 19종은 optional. required 실패 시 ObsAgentDown alert 가 발화 (agent 자체 crash 라 본 메트릭은 emit 도달 불가).
- fake symbol 시뮬 절차: dev cluster 에서 `NETOBS_BPF_FAKE_ATTACH_SYMBOLS=__netobs_nonexistent_probe` env 를 daemonset 에 patch 후 rollout 하면 fake symbol attach 시도가 자연 실패 해 `attach_total{result="failure"}` 와 `attach_retry_total{reason="symbol_not_found"}` 메트릭이 발화. env unset 후 rollout 으로 시뮬 종료. `test/perf/bpf-self-health/verify.sh` 가 본 시뮬 흐름을 자동화 한다.
