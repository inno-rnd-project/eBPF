package correlation

import (
	"math"
	"testing"
)

// 단일 규약 어휘 (#381) 의 환산·순위·환원 매핑을 검증한다.

func TestNodeStatusRank_Ordering(t *testing.T) {
	order := []string{NodeStatusDown, NodeStatusCritical, NodeStatusWarning, NodeStatusHealthy, NodeStatusUnknown}
	for i := 1; i < len(order); i++ {
		if NodeStatusRank(order[i-1]) <= NodeStatusRank(order[i]) {
			t.Errorf("rank(%s)=%d 가 rank(%s)=%d 보다 커야 한다", order[i-1], NodeStatusRank(order[i-1]), order[i], NodeStatusRank(order[i]))
		}
	}
}

func TestNodeStatusFromPressure(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{PressureHighThreshold, NodeStatusCritical},
		{PressureElevatedThreshold, NodeStatusWarning},
		{PressureElevatedThreshold - 0.01, NodeStatusHealthy},
		{0, NodeStatusHealthy},
		{math.NaN(), NodeStatusUnknown},
	}
	for _, c := range cases {
		if got := NodeStatusFromPressure(c.score); got != c.want {
			t.Errorf("NodeStatusFromPressure(%v) = %s, want %s", c.score, got, c.want)
		}
	}
}

func TestNodeStatusFromUsage(t *testing.T) {
	cases := []struct {
		frac float64
		want string
	}{
		{NodeUsageDegradedThreshold, NodeStatusCritical},
		{NodeUsageWarnThreshold, NodeStatusWarning},
		{NodeUsageWarnThreshold - 0.01, NodeStatusHealthy},
	}
	for _, c := range cases {
		if got := NodeStatusFromUsage(c.frac); got != c.want {
			t.Errorf("NodeStatusFromUsage(%v) = %s, want %s", c.frac, got, c.want)
		}
	}
}

func TestNodeStatusFromHealthScore(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{HealthOKThreshold, NodeStatusHealthy},
		{HealthWarnThreshold, NodeStatusWarning},
		{HealthWarnThreshold - 0.01, NodeStatusCritical},
		{math.NaN(), NodeStatusUnknown},
	}
	for _, c := range cases {
		if got := NodeStatusFromHealthScore(c.score); got != c.want {
			t.Errorf("NodeStatusFromHealthScore(%v) = %s, want %s", c.score, got, c.want)
		}
	}
}

// 환원 매핑이 전사적 (규약 어휘 전부가 기존 어휘로 매핑) 이고 등급 순서를 보존하는지 본다.
func TestNodeDetailStatus_TotalMapping(t *testing.T) {
	want := map[string]string{
		NodeStatusCritical: "degraded",
		NodeStatusWarning:  "warn",
		NodeStatusHealthy:  "ok",
		NodeStatusUnknown:  "unknown",
	}
	for unified, detail := range want {
		if got := NodeDetailStatus(unified); got != detail {
			t.Errorf("NodeDetailStatus(%s) = %s, want %s", unified, got, detail)
		}
	}
}
