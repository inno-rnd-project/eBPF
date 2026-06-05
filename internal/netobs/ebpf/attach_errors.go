// Package ebpfx 의 attach_errors.go 는 #105 의 BPF program attach 실패 가시화 를 위한 에러 분류 진입점
// 이다. cilium/ebpf 와 kernel syscall 의 에러 값을 운영자가 이해 가능한 7종 enum 으로 정규화 해 metrics
// 라벨 카디널리티 폭증을 차단 한다. 본 분류 helper 는 후속 BPF program type (tracepoint / fentry / perf_event /
// tc / xdp) 도입 시에도 동일 시그니처 로 재사용 되도록 program type 무관 으로 설계 되었다.
package ebpfx

import (
	"errors"
	"strings"
	"syscall"

	cebpf "github.com/cilium/ebpf"
)

// AttachReason 은 BPF program attach 실패 원인의 정규화 enum 이다. metrics 라벨 값으로 직접 노출 되므로
// 카디널리티 폭증 회피 를 위해 셋이 폐쇄적 으로 관리 된다.
type AttachReason int

const (
	// ReasonOther 는 분류 미매핑 또는 nil 에러 케이스. 신규 에러 매핑이 추가 될 때까지의 임시 슬롯.
	ReasonOther AttachReason = iota
	// ReasonSymbolNotFound 는 kernel symbol lookup 실패 (kprobe attach 대상 함수 부재).
	// kernel 버전 변경 으로 inline 화 / 이름 변경 / 제거 된 경우 가 본 분기 로 흡수 된다.
	ReasonSymbolNotFound
	// ReasonKernelMismatch 는 kernel 자체 가 본 BPF feature (BTF / 특정 helper / program type) 를 미지원.
	// cilium/ebpf 의 ErrNotSupported wrapping 이 본 분기 로 매핑 된다.
	ReasonKernelMismatch
	// ReasonBtfMissing 은 BTF 정보 부재 (/sys/kernel/btf/vmlinux 부재 또는 컨테이너 mount 누락).
	// BTF CO-RE 의존 program 의 load / attach 실패 가 본 분기 로 흡수 된다.
	ReasonBtfMissing
	// ReasonVerifierRejected 는 BPF verifier 가 program 을 거부 (loop / pointer arithmetic / map access 등).
	ReasonVerifierRejected
	// ReasonPermissionDenied 는 CAP_BPF / CAP_PERFMON / CAP_SYS_RESOURCE / CAP_SYS_PTRACE 부족 으로 인한
	// syscall.EPERM 또는 syscall.EACCES.
	ReasonPermissionDenied
	// ReasonLinkInternal 는 cilium/ebpf 의 link.Kprobe / link.Kretprobe 내부 오류 (위 분기 외).
	ReasonLinkInternal
)

// String 은 AttachReason 을 metrics 라벨 값으로 안정 노출할 소문자 snake_case 문자열로 변환한다. enum
// 신규 추가 시에도 기존 dashboard / alert query 가 영향 받지 않도록 매핑 셋을 폐쇄적 으로 유지 한다.
func (r AttachReason) String() string {
	switch r {
	case ReasonSymbolNotFound:
		return "symbol_not_found"
	case ReasonKernelMismatch:
		return "kernel_version_mismatch"
	case ReasonBtfMissing:
		return "btf_missing"
	case ReasonVerifierRejected:
		return "verifier_rejected"
	case ReasonPermissionDenied:
		return "permission_denied"
	case ReasonLinkInternal:
		return "link_internal_error"
	}
	return "other"
}

// AttachReasonValues 는 metrics 패키지 가 카운터 등록 단계 에서 가능한 라벨 값을 사전 등록 하기 위한
// enum 셋 이다. 본 슬라이스가 enum 추가 시 함께 갱신 되어야 cardinality 사전 등록 invariant 가 유지된다.
var AttachReasonValues = []AttachReason{
	ReasonSymbolNotFound,
	ReasonKernelMismatch,
	ReasonBtfMissing,
	ReasonVerifierRejected,
	ReasonPermissionDenied,
	ReasonLinkInternal,
	ReasonOther,
}

// classifyAttachError 는 cilium/ebpf 와 kernel syscall 에서 발생한 attach 실패 에러 를 7종 AttachReason
// enum 으로 분류 한다. errors.Is / errors.As 를 우선 사용 해 wrapping 깊이 와 무관 한 일관 분류 를 보장
// 하고, 표준 wrapping 으로 식별 안되는 경우 에러 메시지 substring 매칭 으로 폴백 한다 (cilium/ebpf 가
// 정형 에러 타입을 노출 하지 않는 일부 verifier 경로 흡수 목적). nil 에러 는 ReasonOther 로 반환 되며
// 호출 측이 success 경로 와 분기 해야 한다.
func classifyAttachError(err error) AttachReason {
	if err == nil {
		return ReasonOther
	}

	// 1) cilium/ebpf 의 명시적 분류 에러 우선.
	if errors.Is(err, cebpf.ErrNotSupported) {
		return ReasonKernelMismatch
	}

	// 2) syscall 단 에러 매핑. unix.ENOENT 가 kprobe attach 의 symbol 부재 시그널 로 자주 사용 됨.
	if errors.Is(err, syscall.ENOENT) {
		return ReasonSymbolNotFound
	}
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		return ReasonPermissionDenied
	}

	// 3) verifier / BTF 관련 에러 는 cilium/ebpf 가 정형 타입 으로 노출 하지 않는 경로 가 있어 메시지
	//    substring 매칭 으로 흡수. kernel 메시지가 안정적 이라 카디널리티 영향 없음.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "verifier") || (strings.Contains(msg, "invalid argument") && strings.Contains(msg, "bpf")) {
		return ReasonVerifierRejected
	}
	if strings.Contains(msg, "btf") {
		return ReasonBtfMissing
	}

	// 4) cilium/ebpf link 패키지 내부 오류 의 흡수 슬롯. 위 분기 모두 미해당 시 link_internal_error.
	if strings.Contains(msg, "link") || strings.Contains(msg, "perf_event_open") || strings.Contains(msg, "tracefs") {
		return ReasonLinkInternal
	}

	return ReasonOther
}
