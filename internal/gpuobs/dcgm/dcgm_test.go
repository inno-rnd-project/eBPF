package dcgm

import "testing"

// TestNoopSource_Available 은 noopSource 가 항상 false 를 돌려 주어 dev cluster 의 RTX 3090 환경
// 에서 graceful degradation 식별 진입점 으로 동작 하는지 회귀 가드 한다.
func TestNoopSource_Available(t *testing.T) {
	s := NewNoop()
	if s.Available() {
		t.Errorf("noopSource.Available()=true want false")
	}
}

// TestNoopSource_MetricForward 는 noopSource 가 prefix 와 무관 하게 빈 슬라이스 를 돌려 주는지
// 검증 한다. 빈 슬라이스 반환 으로 wire-up 측 의 nil dereference 위험 도 함께 차단 된다.
func TestNoopSource_MetricForward(t *testing.T) {
	s := NewNoop()
	for _, prefix := range []string{"", "dcgm:", "nvlink:"} {
		got := s.MetricForward(prefix)
		if len(got) != 0 {
			t.Errorf("noopSource.MetricForward(%q)=%d samples want 0", prefix, len(got))
		}
	}
}

// TestNoopSource_Close 는 noopSource 의 Close 가 정상 nil 반환 인지 검증 한다. cmd/gpuobs-agent
// 의 shutdown 흐름 에서 defer Close 가 panic 없이 종료 되는 회귀 가드 다.
func TestNoopSource_Close(t *testing.T) {
	s := NewNoop()
	if err := s.Close(); err != nil {
		t.Errorf("noopSource.Close()=%v want nil", err)
	}
}
