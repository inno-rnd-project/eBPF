package ebpfx

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	cebpf "github.com/cilium/ebpf"
)

// TestClassifyAttachError_NilReturnsOther 는 nil 에러 입력의 정상 흡수 검증. 호출 측 의 잘못된 진입을
// panic / 미정의 enum 으로 노출 하지 않고 ReasonOther 로 안전 반환 한다.
func TestClassifyAttachError_NilReturnsOther(t *testing.T) {
	if got := classifyAttachError(nil); got != ReasonOther {
		t.Errorf("classifyAttachError(nil)=%v want ReasonOther", got)
	}
}

// TestClassifyAttachError_SymbolNotFound 는 syscall.ENOENT (kprobe attach 의 symbol lookup 실패 시그널)
// 가 ReasonSymbolNotFound 로 정확히 분류 되는지 검증. errors.Is 기반 wrapping 깊이 무관.
func TestClassifyAttachError_SymbolNotFound(t *testing.T) {
	cases := []error{
		syscall.ENOENT,
		fmt.Errorf("kprobe: %w", syscall.ENOENT),
		fmt.Errorf("attach %s: kprobe nonexistent symbol: %w", "tcp_foobar", syscall.ENOENT),
	}
	for i, err := range cases {
		if got := classifyAttachError(err); got != ReasonSymbolNotFound {
			t.Errorf("case %d: classifyAttachError(%v)=%v want ReasonSymbolNotFound", i, err, got)
		}
	}
}

// TestClassifyAttachError_PermissionDenied 는 EPERM / EACCES 모두 ReasonPermissionDenied 로 흡수 검증.
func TestClassifyAttachError_PermissionDenied(t *testing.T) {
	cases := []error{
		syscall.EPERM,
		syscall.EACCES,
		fmt.Errorf("perf_event_open: %w", syscall.EPERM),
	}
	for i, err := range cases {
		if got := classifyAttachError(err); got != ReasonPermissionDenied {
			t.Errorf("case %d: classifyAttachError(%v)=%v want ReasonPermissionDenied", i, err, got)
		}
	}
}

// TestClassifyAttachError_KernelMismatch 는 cilium/ebpf 의 ErrNotSupported (kernel 미지원) 흡수 검증.
func TestClassifyAttachError_KernelMismatch(t *testing.T) {
	if got := classifyAttachError(cebpf.ErrNotSupported); got != ReasonKernelMismatch {
		t.Errorf("ErrNotSupported direct=%v want ReasonKernelMismatch", got)
	}
	wrapped := fmt.Errorf("load: %w", cebpf.ErrNotSupported)
	if got := classifyAttachError(wrapped); got != ReasonKernelMismatch {
		t.Errorf("ErrNotSupported wrapped=%v want ReasonKernelMismatch", got)
	}
}

// TestClassifyAttachError_VerifierRejected 는 BPF verifier 에러 메시지의 substring 매칭 분류.
func TestClassifyAttachError_VerifierRejected(t *testing.T) {
	cases := []error{
		errors.New("verifier returned 13: invalid mem access"),
		errors.New("load program: bpf invalid argument"),
	}
	for i, err := range cases {
		if got := classifyAttachError(err); got != ReasonVerifierRejected {
			t.Errorf("case %d: classifyAttachError(%v)=%v want ReasonVerifierRejected", i, err, got)
		}
	}
}

// TestClassifyAttachError_BtfMissing 는 BTF 부재 에러 메시지의 substring 매칭 분류.
func TestClassifyAttachError_BtfMissing(t *testing.T) {
	err := errors.New("relocation against external symbol: btf not found")
	if got := classifyAttachError(err); got != ReasonBtfMissing {
		t.Errorf("classifyAttachError(%v)=%v want ReasonBtfMissing", err, got)
	}
}

// TestClassifyAttachError_LinkInternal 는 cilium/ebpf link 패키지 내부 오류 흡수 검증.
func TestClassifyAttachError_LinkInternal(t *testing.T) {
	cases := []error{
		errors.New("link create: i/o error"),
		errors.New("perf_event_open: file descriptor exhausted"),
		errors.New("tracefs: read denied"),
	}
	for i, err := range cases {
		if got := classifyAttachError(err); got != ReasonLinkInternal {
			t.Errorf("case %d: classifyAttachError(%v)=%v want ReasonLinkInternal", i, err, got)
		}
	}
}

// TestClassifyAttachError_OtherFallback 는 위 분기 모두 미해당 에러 가 ReasonOther 로 흡수 검증.
func TestClassifyAttachError_OtherFallback(t *testing.T) {
	if got := classifyAttachError(errors.New("totally unknown failure mode")); got != ReasonOther {
		t.Errorf("unknown error=%v want ReasonOther", got)
	}
}

// TestAttachReason_StringEnum 은 7종 enum 의 String() 라벨 값이 dashboard / alert query 에서 참조되는
// 폐쇄 셋과 일치 하는지 검증. 신규 enum 추가 시 본 테스트가 깨지므로 매핑 누락 차단 가드 역할.
func TestAttachReason_StringEnum(t *testing.T) {
	expected := map[AttachReason]string{
		ReasonSymbolNotFound:   "symbol_not_found",
		ReasonKernelMismatch:   "kernel_version_mismatch",
		ReasonBtfMissing:       "btf_missing",
		ReasonVerifierRejected: "verifier_rejected",
		ReasonPermissionDenied: "permission_denied",
		ReasonLinkInternal:     "link_internal_error",
		ReasonOther:            "other",
	}
	if len(AttachReasonValues) != len(expected) {
		t.Fatalf("AttachReasonValues len=%d want %d (enum 추가 시 본 슬라이스도 함께 갱신)", len(AttachReasonValues), len(expected))
	}
	for r, want := range expected {
		if got := r.String(); got != want {
			t.Errorf("AttachReason(%d).String()=%q want %q", r, got, want)
		}
	}
}
