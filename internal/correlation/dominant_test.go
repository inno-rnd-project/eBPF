package correlation

import "testing"

// TestComputeDominantDimension_SingleDominant 는 한 dimension 에 score 가 몰린 victim 의 dominant
// 가 그 dimension 으로 정확히 채택되는지 검증한다.
func TestComputeDominantDimension_SingleDominant(t *testing.T) {
	neighbors := []NoisyNeighbor{
		{
			Victim:       PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"},
			VictimSignal: SignalLatency,
			Dimension:    DimensionCPU,
			Score:        0.9,
		},
		{
			Victim:       PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"},
			VictimSignal: SignalLatency,
			Dimension:    DimensionMemory,
			Score:        0.1,
		},
	}
	got := ComputeDominantDimension(neighbors)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].Dimension != DimensionCPU {
		t.Errorf("dimension=%s want cpu", got[0].Dimension)
	}
}

// TestComputeDominantDimension_TieBreakerEnumOrder 는 정확 동률 시 enum 사전순 가장 앞 (cpu) 이
// 채택되는지 검증한다. 노출되는 Weight 는 offset 가산 없는 raw 정규화 값 (0.25) 이어야 한다.
func TestComputeDominantDimension_TieBreakerEnumOrder(t *testing.T) {
	neighbors := []NoisyNeighbor{
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionCPU, Score: 0.5},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionGPU, Score: 0.5},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionMemory, Score: 0.5},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionNetwork, Score: 0.5},
	}
	got := ComputeDominantDimension(neighbors)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].Dimension != DimensionCPU {
		t.Errorf("dimension=%s want cpu (사전순 가장 앞)", got[0].Dimension)
	}
	if got[0].Weight != 0.25 {
		t.Errorf("weight=%g want 0.25 (offset 가산 없는 raw 정규화 값)", got[0].Weight)
	}
}

// TestComputeDominantDimension_WeightIsRawNormalized 는 dominant Weight 에 offset 이 가산되지
// 않아 [0, 1] 범위 raw 정규화 값으로 노출되는지 검증한다. dashboard 와 alert 가 본 raw 값을 그대로
// 사용한다.
func TestComputeDominantDimension_WeightIsRawNormalized(t *testing.T) {
	neighbors := []NoisyNeighbor{
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionCPU, Score: 0.8},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionMemory, Score: 0.2},
	}
	got := ComputeDominantDimension(neighbors)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	// CPU score 0.8 + memory score 0.2 = sum 1.0. cpu weight = 0.8.
	if got[0].Weight != 0.8 {
		t.Errorf("weight=%g want 0.8 (offset 제외)", got[0].Weight)
	}
	if got[0].Weight > 1.0 {
		t.Errorf("weight=%g exceeds 1.0", got[0].Weight)
	}
}

// TestComputeDominantDimension_ZeroSumSkipped 는 4 dimension 모두 score 가 0 인 victim 은 결과에서
// 제외되는지 검증한다. 분모 가드의 dashboard 빈 시리즈 차단 동작.
func TestComputeDominantDimension_ZeroSumSkipped(t *testing.T) {
	neighbors := []NoisyNeighbor{
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionCPU, Score: 0},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionMemory, Score: 0},
	}
	got := ComputeDominantDimension(neighbors)
	if len(got) != 0 {
		t.Errorf("len=%d want 0 (모든 score 0)", len(got))
	}
}

// TestComputeDominantDimension_MultipleVictims 는 victim 별로 dominant 가 독립 산정되는지 검증한다.
func TestComputeDominantDimension_MultipleVictims(t *testing.T) {
	neighbors := []NoisyNeighbor{
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionCPU, Score: 0.9},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, VictimSignal: SignalLatency, Dimension: DimensionMemory, Score: 0.1},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v2", PodUID: "u2"}, VictimSignal: SignalLatency, Dimension: DimensionGPU, Score: 0.8},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v2", PodUID: "u2"}, VictimSignal: SignalLatency, Dimension: DimensionNetwork, Score: 0.2},
	}
	got := ComputeDominantDimension(neighbors)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Victim.Pod != "v1" || got[0].Dimension != DimensionCPU {
		t.Errorf("v1: dim=%s want cpu", got[0].Dimension)
	}
	if got[1].Victim.Pod != "v2" || got[1].Dimension != DimensionGPU {
		t.Errorf("v2: dim=%s want gpu", got[1].Dimension)
	}
}

// TestComputeDominantDimension_MaxScorePerDimension 은 같은 dimension 에 여러 suspect 가 있을 때
// max score 가 dominant 산정 입력으로 들어가는지 검증한다.
func TestComputeDominantDimension_MaxScorePerDimension(t *testing.T) {
	neighbors := []NoisyNeighbor{
		// cpu dimension 두 suspect: max=0.9
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, Suspect: PodIdentity{Pod: "s1"}, VictimSignal: SignalLatency, Dimension: DimensionCPU, Score: 0.4},
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, Suspect: PodIdentity{Pod: "s2"}, VictimSignal: SignalLatency, Dimension: DimensionCPU, Score: 0.9},
		// memory dimension 한 suspect: 0.5
		{Victim: PodIdentity{Namespace: "ns", Pod: "v1", PodUID: "u1"}, Suspect: PodIdentity{Pod: "s3"}, VictimSignal: SignalLatency, Dimension: DimensionMemory, Score: 0.5},
	}
	got := ComputeDominantDimension(neighbors)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].Dimension != DimensionCPU {
		t.Errorf("dimension=%s want cpu (max=0.9)", got[0].Dimension)
	}
}
