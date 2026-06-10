package sources

import (
	"math"
	"testing"
)

// TestComputeConfidenceScore_AllSources 는 세 source 가 모두 최대 신호 (1.0) 일 때 confidence 가
// 1.0 에 도달 하는지 회귀 가드 한다. 가중치 합산 (0.5 + 0.3 + 0.2 = 1.0) 이 정합 한지 확인 한다.
func TestComputeConfidenceScore_AllSources(t *testing.T) {
	got := ComputeConfidenceScore(ConfidenceFactors{
		Correlation: 1.0,
		Netobs:      1.0,
		Gpuobs:      1.0,
	})
	if math.Abs(got-1.0) > 1e-9 {
		t.Errorf("ComputeConfidenceScore(all=1.0)=%v want 1.0", got)
	}
}

// TestComputeConfidenceScore_CorrelationOnly 는 correlation source 만 최대 신호 (1.0) 일 때
// confidence 가 weight (0.5) 에 머무는지 확인 한다. 단일 source 만 으로 false positive guard
// threshold (0.3) 통과 가 보장 되는 정책 의 회귀 가드 다.
func TestComputeConfidenceScore_CorrelationOnly(t *testing.T) {
	got := ComputeConfidenceScore(ConfidenceFactors{Correlation: 1.0})
	if math.Abs(got-WeightCorrelation) > 1e-9 {
		t.Errorf("ComputeConfidenceScore(correlation=1.0)=%v want %v", got, WeightCorrelation)
	}
}

// TestComputeConfidenceScore_Empty 는 모든 source 가 빈 신호 (0) 일 때 confidence 가 0 인지
// 확인 한다. false positive guard threshold 미만 으로 RCA emit 이 skip 되는 케이스 다.
func TestComputeConfidenceScore_Empty(t *testing.T) {
	got := ComputeConfidenceScore(ConfidenceFactors{})
	if got != 0 {
		t.Errorf("ComputeConfidenceScore(empty)=%v want 0", got)
	}
}

// TestComputeConfidenceScore_Clamp 는 factor 가 0-1 범위 밖일 때 clamp 가 정상 동작 하는지
// 검증 한다. 음수 는 0 으로, 1.0 초과 는 1.0 으로 정규화 되어 confidence 가 항상 0-1 범위에
// 머문다.
func TestComputeConfidenceScore_Clamp(t *testing.T) {
	got := ComputeConfidenceScore(ConfidenceFactors{
		Correlation: 1.5,
		Netobs:      -0.3,
		Gpuobs:      0.5,
	})
	want := WeightCorrelation*1.0 + WeightNetobs*0 + WeightGpuobs*0.5
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("ComputeConfidenceScore(clamp)=%v want %v", got, want)
	}
}

// TestComputeConfidenceScore_BelowThreshold 는 단일 source 의 약한 신호 만 으로는 confidence 가
// false positive guard threshold (0.3) 미만 으로 떨어지는 케이스 의 회귀 가드 다.
func TestComputeConfidenceScore_BelowThreshold(t *testing.T) {
	got := ComputeConfidenceScore(ConfidenceFactors{Correlation: 0.4})
	if got >= 0.3 {
		t.Errorf("ComputeConfidenceScore(weak correlation)=%v want < 0.3", got)
	}
}
