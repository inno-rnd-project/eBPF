package blastradius

import (
	"math"
	"reflect"
	"testing"
)

// TestCompute_NormalIncrease 는 impact 가 baseline 보다 커진 정상 cases 에서 score 가 정확한 비율
// 로 산출되는지 검증한다.
func TestCompute_NormalIncrease(t *testing.T) {
	cases := []struct {
		name     string
		baseline []float64
		impact   []float64
		want     float64
	}{
		{"50% increase", []float64{0.01, 0.01, 0.01}, []float64{0.015, 0.015, 0.015}, 0.5},
		{"100% increase clamped to 1", []float64{0.01}, []float64{0.02}, 1.0},
		{"200% increase clamped to 1", []float64{0.01}, []float64{0.03}, 1.0},
		{"identical (no impact)", []float64{0.01}, []float64{0.01}, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, status := Compute(tc.baseline, tc.impact)
			if status != StatusOK {
				t.Fatalf("status=%s want ok", status)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("score=%v want %v", got, tc.want)
			}
		})
	}
}

// TestCompute_ImprovementClampedToZero 는 impact 가 baseline 보다 작아져도 score 가 음수가 아닌
// 0 으로 clamp 되는지 검증한다. improvement 는 noisy neighbor 모델에 부합하지 않는다.
func TestCompute_ImprovementClampedToZero(t *testing.T) {
	got, status := Compute([]float64{0.02}, []float64{0.01})
	if status != StatusOK {
		t.Fatalf("status=%s want ok", status)
	}
	if got != 0 {
		t.Errorf("score=%v want 0 (improvement 은 0 clamp)", got)
	}
}

// TestCompute_LowBaselineSkipped 는 baseline 평균이 minBaselineThreshold 미만일 때 산출이 skip
// 되는지 검증한다. 트래픽이 거의 없는 victim 이 본 status 로 분류된다.
func TestCompute_LowBaselineSkipped(t *testing.T) {
	got, status := Compute([]float64{1e-7, 1e-7}, []float64{1.0})
	if status != StatusSkippedLowBaseline {
		t.Errorf("status=%s want skipped_low_baseline", status)
	}
	if got != 0 {
		t.Errorf("score=%v want 0", got)
	}
}

// TestCompute_NoSamplesSkipped 는 NaN / Inf 제거 후 표본 수가 0 인 입력이 정상적으로 skip 되는지
// 검증한다.
func TestCompute_NoSamplesSkipped(t *testing.T) {
	cases := []struct {
		name     string
		baseline []float64
		impact   []float64
	}{
		{"empty baseline", []float64{}, []float64{0.01}},
		{"empty impact", []float64{0.01}, []float64{}},
		{"all NaN baseline", []float64{math.NaN(), math.NaN()}, []float64{0.01}},
		{"all Inf impact", []float64{0.01}, []float64{math.Inf(1), math.Inf(-1)}},
		{"both empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, status := Compute(tc.baseline, tc.impact)
			if status != StatusSkippedNoSamples {
				t.Errorf("status=%s want skipped_no_samples", status)
			}
			if got != 0 {
				t.Errorf("score=%v want 0", got)
			}
		})
	}
}

// TestCompute_NaNPairwiseRemoval 은 NaN / Inf 가 일부 포함된 입력에서 유효 표본만으로 산출이 되는
// 지 검증한다.
func TestCompute_NaNPairwiseRemoval(t *testing.T) {
	baseline := []float64{0.01, math.NaN(), 0.01, math.Inf(1)}
	impact := []float64{0.015, 0.015, math.NaN(), 0.015}
	got, status := Compute(baseline, impact)
	if status != StatusOK {
		t.Fatalf("status=%s want ok", status)
	}
	if math.Abs(got-0.5) > 1e-9 {
		t.Errorf("score=%v want 0.5", got)
	}
}

// TestVictimCandidates_SameNodeOnly 는 같은 노드의 다른 Pod 만 채택하고 cross-node 와 self 는
// 제외하는지 검증한다.
func TestVictimCandidates_SameNodeOnly(t *testing.T) {
	all := []VictimCandidate{
		{Namespace: "default", Pod: "target", PodUID: "uid-t", Node: "n1"},
		{Namespace: "default", Pod: "neighbor", PodUID: "uid-n", Node: "n1"},
		{Namespace: "other", Pod: "cross-node", PodUID: "uid-c", Node: "n2"},
	}
	got := VictimCandidates(all, "n1", "default", "target", 0)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].Pod != "neighbor" {
		t.Errorf("got=%s want neighbor", got[0].Pod)
	}
}

// TestVictimCandidates_DeterministicOrder 는 같은 입력에 대해 결과 순서가 결정적인지 검증한다.
// map 순회 비결정성이 회귀로 들어오지 않게 가드.
func TestVictimCandidates_DeterministicOrder(t *testing.T) {
	all := []VictimCandidate{
		{Namespace: "ns-b", Pod: "p2", PodUID: "u2", Node: "n1"},
		{Namespace: "ns-a", Pod: "p1", PodUID: "u1", Node: "n1"},
		{Namespace: "ns-a", Pod: "p3", PodUID: "u3", Node: "n1"},
	}
	first := VictimCandidates(all, "n1", "default", "target", 0)
	for i := 0; i < 10; i++ {
		next := VictimCandidates(all, "n1", "default", "target", 0)
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("non-deterministic order")
		}
	}
}

// TestVictimCandidates_MaxVictims 는 maxVictims 양수가 정렬 후 상위 N 만 채택하도록 cap 하는지
// 검증한다.
func TestVictimCandidates_MaxVictims(t *testing.T) {
	all := []VictimCandidate{
		{Namespace: "ns-a", Pod: "p1", Node: "n1"},
		{Namespace: "ns-a", Pod: "p2", Node: "n1"},
		{Namespace: "ns-a", Pod: "p3", Node: "n1"},
	}
	got := VictimCandidates(all, "n1", "default", "target", 2)
	if len(got) != 2 {
		t.Errorf("len=%d want 2", len(got))
	}
}
