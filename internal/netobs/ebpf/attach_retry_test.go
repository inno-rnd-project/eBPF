package ebpfx

import (
	"errors"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/cilium/ebpf/link"
)

// TestAttachWithRetry_FirstAttemptSuccess 는 fn 이 첫 시도에 성공 하면 retry 없이 즉시 반환 되는지 검증.
// retry budget / backoff 비용 zero invariant.
func TestAttachWithRetry_FirstAttemptSuccess(t *testing.T) {
	var calls atomic.Int32
	_, err := attachWithRetry("test_first_success", func() (link.Link, error) {
		calls.Add(1)
		return nil, nil // nil link 반환 도 본 helper 가 wrap 하지 않아 OK.
	})
	if err != nil {
		t.Errorf("err=%v want nil", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fn calls=%d want 1 (first-attempt success)", got)
	}
}

// TestAttachWithRetry_EventualSuccessAfterRetries 는 2회 실패 후 3번째 시도에서 성공 하면 retry budget
// 내 정상 반환 되는지 검증. attach_total{result="success"} 와 attach_retry_total 누적 의미가 함께 만족.
func TestAttachWithRetry_EventualSuccessAfterRetries(t *testing.T) {
	var calls atomic.Int32
	_, err := attachWithRetry("test_eventual_success", func() (link.Link, error) {
		n := calls.Add(1)
		if n < 3 {
			return nil, syscall.ENOENT
		}
		return nil, nil
	})
	if err != nil {
		t.Errorf("err=%v want nil after 2 retries", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("fn calls=%d want 3 (2 retries + 1 success)", got)
	}
}

// TestAttachWithRetry_ExhaustsBudgetThenFails 는 max retries 까지 모두 실패 시 마지막 에러를 그대로
// 반환 하고 호출 횟수 가 maxRetries+1 (= 4) 인지 검증. attach_total{result="failure"} 단발 emit 의미.
func TestAttachWithRetry_ExhaustsBudgetThenFails(t *testing.T) {
	var calls atomic.Int32
	target := errors.New("persistent failure")
	_, err := attachWithRetry("test_exhausted", func() (link.Link, error) {
		calls.Add(1)
		return nil, target
	})
	if !errors.Is(err, target) {
		t.Errorf("err=%v want %v (last error propagated)", err, target)
	}
	if got := calls.Load(); got != attachMaxRetries+1 {
		t.Errorf("fn calls=%d want %d (max retries + first attempt)", got, attachMaxRetries+1)
	}
}

// TestFakeAttachSymbols_ParsesCommaSeparated 는 NETOBS_BPF_FAKE_ATTACH_SYMBOLS env 의 파싱 검증.
// 빈 토큰 / 공백 trim / 빈 입력 케이스를 모두 정상 흡수 해 verify.sh 의 injection 진입점 invariant 보장.
func TestFakeAttachSymbols_ParsesCommaSeparated(t *testing.T) {
	cases := []struct {
		env  string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"sym_a", []string{"sym_a"}},
		{"sym_a,sym_b,sym_c", []string{"sym_a", "sym_b", "sym_c"}},
		{" sym_a , , sym_b ", []string{"sym_a", "sym_b"}},
	}
	for i, tc := range cases {
		t.Setenv("NETOBS_BPF_FAKE_ATTACH_SYMBOLS", tc.env)
		got := fakeAttachSymbols()
		if len(got) != len(tc.want) {
			t.Errorf("case %d env=%q got len=%d want %d", i, tc.env, len(got), len(tc.want))
			continue
		}
		for j := range got {
			if got[j] != tc.want[j] {
				t.Errorf("case %d env=%q [%d]=%q want %q", i, tc.env, j, got[j], tc.want[j])
			}
		}
	}
}
