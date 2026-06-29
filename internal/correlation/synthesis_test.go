package correlation

import (
	"math"
	"testing"
)

// TestHealthStatus 는 health score 임계 (0.8 ok / 0.5 warn) 와 NaN(unknown) 경계 분류를 검증한다.
func TestHealthStatus(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{1.0, "ok"}, {0.8, "ok"}, {0.79, "warn"}, {0.5, "warn"}, {0.49, "degraded"}, {0, "degraded"},
		{math.NaN(), "unknown"},
	}
	for _, c := range cases {
		if got := HealthStatus(c.score); got != c.want {
			t.Errorf("HealthStatus(%v)=%q want %q", c.score, got, c.want)
		}
	}
}

// TestPressureSeverity 는 pressure score 임계 (0.7 high / 0.4 elevated) 와 NaN(unknown) 분류를 검증한다.
func TestPressureSeverity(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{1.0, "high"}, {0.7, "high"}, {0.69, "elevated"}, {0.4, "elevated"}, {0.39, "low"}, {0, "low"},
		{math.NaN(), "unknown"},
	}
	for _, c := range cases {
		if got := PressureSeverity(c.score); got != c.want {
			t.Errorf("PressureSeverity(%v)=%q want %q", c.score, got, c.want)
		}
	}
}

// TestZScoreSeverity 는 z-score 절대값 임계 (3 high / 2 elevated) 와 부호 무관성, NaN(none) 을 검증한다.
func TestZScoreSeverity(t *testing.T) {
	cases := []struct {
		z    float64
		want string
	}{
		{3.5, "high"}, {-3.5, "high"}, {3.0, "high"}, {2.5, "elevated"}, {-2.0, "elevated"},
		{1.9, "none"}, {0, "none"}, {math.NaN(), "none"},
	}
	for _, c := range cases {
		if got := ZScoreSeverity(c.z); got != c.want {
			t.Errorf("ZScoreSeverity(%v)=%q want %q", c.z, got, c.want)
		}
	}
}
