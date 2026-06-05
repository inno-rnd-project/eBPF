package ebpfx

import (
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf/link"
)

// withFastRetry 는 G2 fix 후 var 로 노출된 backoff / budget 을 테스트 동안 매우 짧은 값 으로 override
// 해 retry 흐름 을 거의 즉시 검증 가능 하게 한다. 운영 값 (500ms × 5s) 그대로 면 CI 가 매 케이스 마다
// 수 초 대기 하므로 피드백 루프 가 느려진다. t.Cleanup 으로 원복 보장.
func withFastRetry(t *testing.T) {
	t.Helper()
	origBackoff := attachRetryBackoff
	origBudget := attachTotalBudget
	attachRetryBackoff = 1 * time.Millisecond
	attachTotalBudget = 50 * time.Millisecond
	t.Cleanup(func() {
		attachRetryBackoff = origBackoff
		attachTotalBudget = origBudget
	})
}

// TestAttachWithRetry_FirstAttemptSuccess 는 fn 이 첫 시도에 성공 하면 retry 없이 즉시 반환 되는지 검증.
// retry budget / backoff 비용 zero invariant.
func TestAttachWithRetry_FirstAttemptSuccess(t *testing.T) {
	withFastRetry(t)
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
	withFastRetry(t)
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
	withFastRetry(t)
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

// TestAttachWithRetry_BudgetExceededShortCircuit 는 G2 fix 후 짧은 budget override 시 max retries 도달
// 전에 deadline 으로 break 되는 경로 검증. budget 가 backoff 보다 작 으면 첫 시도 실패 후 즉시 종료.
func TestAttachWithRetry_BudgetExceededShortCircuit(t *testing.T) {
	origBackoff := attachRetryBackoff
	origBudget := attachTotalBudget
	attachRetryBackoff = 100 * time.Millisecond
	attachTotalBudget = 1 * time.Millisecond
	t.Cleanup(func() {
		attachRetryBackoff = origBackoff
		attachTotalBudget = origBudget
	})

	var calls atomic.Int32
	_, err := attachWithRetry("test_budget_exceeded", func() (link.Link, error) {
		calls.Add(1)
		return nil, syscall.ENOENT
	})
	if err == nil {
		t.Fatal("err=nil want non-nil (budget exceeded)")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("fn calls=%d want 1 (budget exceeded after first attempt, no retry)", got)
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
