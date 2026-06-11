package nccl

import "testing"

// TestNoopProfiler_Available은 noopProfiler가 항상 false를 돌려주어 dev cluster의 RTX 3090
// 환경에서 graceful degradation 식별 진입점으로 동작하는지 회귀 가드한다.
func TestNoopProfiler_Available(t *testing.T) {
	p := NewNoop()
	if p.Available() {
		t.Errorf("noopProfiler.Available()=true want false")
	}
}

// TestNoopProfiler_Attach는 noopProfiler의 Attach가 정상 nil 반환인지 검증한다. wire-up
// 측의 Attach 호출이 panic 없이 진행되는 회귀 가드다.
func TestNoopProfiler_Attach(t *testing.T) {
	p := NewNoop()
	if err := p.Attach(); err != nil {
		t.Errorf("noopProfiler.Attach()=%v want nil", err)
	}
}

// TestNoopProfiler_EventsClosed는 noopProfiler의 Events가 즉시 close된 channel을 돌려주어
// 호출자의 range 루프가 정상 종료하는지 회귀 가드한다. 미통합 환경에서도 event 소비자의
// goroutine 누수 위험이 차단된다.
func TestNoopProfiler_EventsClosed(t *testing.T) {
	p := NewNoop()
	ch := p.Events()
	count := 0
	for range ch {
		count++
		if count > 0 {
			break
		}
	}
	if count != 0 {
		t.Errorf("noopProfiler.Events() emitted %d events want 0 (closed channel)", count)
	}
}

// TestNoopProfiler_Close는 noopProfiler의 Close가 정상 nil 반환인지 검증한다. shutdown
// 흐름의 defer Close가 panic 없이 종료되는 회귀 가드다.
func TestNoopProfiler_Close(t *testing.T) {
	p := NewNoop()
	if err := p.Close(); err != nil {
		t.Errorf("noopProfiler.Close()=%v want nil", err)
	}
}
