package dcgm

import "testing"

// TestNoopSource_Available은 noopSource가 항상 false를 돌려주어 dev cluster의 RTX 3090 환경
// 에서 graceful degradation 식별 진입점으로 동작하는지 회귀 가드한다.
func TestNoopSource_Available(t *testing.T) {
	s := NewNoop()
	if s.Available() {
		t.Errorf("noopSource.Available()=true want false")
	}
}

// TestNoopSource_MetricForward는 noopSource가 prefix와 무관하게 빈 슬라이스를 돌려주는지
// 검증한다. 빈 슬라이스 반환으로 wire-up 측의 nil dereference 위험도 함께 차단된다.
func TestNoopSource_MetricForward(t *testing.T) {
	s := NewNoop()
	for _, prefix := range []string{"", "dcgm:", "nvlink:"} {
		got := s.MetricForward(prefix)
		if len(got) != 0 {
			t.Errorf("noopSource.MetricForward(%q)=%d samples want 0", prefix, len(got))
		}
	}
}

// TestNoopSource_Close는 noopSource의 Close가 정상 nil 반환인지 검증한다. cmd/gpuobs-agent
// 의 shutdown 흐름에서 defer Close가 panic 없이 종료되는 회귀 가드다.
func TestNoopSource_Close(t *testing.T) {
	s := NewNoop()
	if err := s.Close(); err != nil {
		t.Errorf("noopSource.Close()=%v want nil", err)
	}
}
