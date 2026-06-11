package nccl

import "testing"

// TestNoopProfiler_Available 은 noopProfiler 가 항상 false 를 돌려 주어 dev cluster 의 RTX 3090
// 환경 에서 graceful degradation 식별 진입점 으로 동작 하는지 회귀 가드 한다.
func TestNoopProfiler_Available(t *testing.T) {
	p := NewNoop()
	if p.Available() {
		t.Errorf("noopProfiler.Available()=true want false")
	}
}

// TestNoopProfiler_Attach 는 noopProfiler 의 Attach 가 정상 nil 반환 인지 검증 한다. wire-up
// 측 의 Attach 호출 이 panic 없이 진행 되는 회귀 가드 다.
func TestNoopProfiler_Attach(t *testing.T) {
	p := NewNoop()
	if err := p.Attach(); err != nil {
		t.Errorf("noopProfiler.Attach()=%v want nil", err)
	}
}

// TestNoopProfiler_EventsClosed 는 noopProfiler 의 Events 가 즉시 close 된 channel 을 돌려 주어
// 호출 자 의 range 루프 가 정상 종료 하는지 회귀 가드 한다. 미통합 환경 에서도 event 소비 자 의
// goroutine 누수 위험 이 차단 된다.
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

// TestNoopProfiler_Close 는 noopProfiler 의 Close 가 정상 nil 반환 인지 검증 한다. shutdown
// 흐름 의 defer Close 가 panic 없이 종료 되는 회귀 가드 다.
func TestNoopProfiler_Close(t *testing.T) {
	p := NewNoop()
	if err := p.Close(); err != nil {
		t.Errorf("noopProfiler.Close()=%v want nil", err)
	}
}
