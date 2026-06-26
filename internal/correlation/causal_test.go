package correlation

import (
	"math"
	"testing"
)

// TestComputeCausalStrength_Range 는 #176 의 핵심 수용 조건이다. Pearson 강도와 Granger p-value 와
// effect p-value 의 경계값 / 비정상 입력 (음수, 1 초과, NaN) 조합에서도 결합 지표가 항상 [0,1] 범위를
// 유지하는지 입증한다.
func TestComputeCausalStrength_Range(t *testing.T) {
	vals := []float64{-0.5, 0, 0.3, 0.5, 1, 1.5, math.NaN()}
	bools := []bool{false, true}
	for _, s := range vals {
		for _, gok := range bools {
			for _, gp := range vals {
				for _, eok := range bools {
					for _, ep := range vals {
						cs := ComputeCausalStrength(CausalFactors{
							PearsonStrength: s,
							GrangerOK:       gok,
							GrangerPValue:   gp,
							EffectOK:        eok,
							EffectPValue:    ep,
						})
						if cs < 0 || cs > 1 || math.IsNaN(cs) {
							t.Fatalf("causal_strength=%v out of [0,1] for s=%v gok=%v gp=%v eok=%v ep=%v", cs, s, gok, gp, eok, ep)
						}
					}
				}
			}
		}
	}
}

// TestComputeCausalStrength_AllStrong 은 세 신호가 모두 최대 (강한 상관, 유의한 Granger, 유의한 effect)
// 일 때 인과강도가 1 에 수렴하는지 검증한다.
func TestComputeCausalStrength_AllStrong(t *testing.T) {
	cs := ComputeCausalStrength(CausalFactors{
		PearsonStrength: 1,
		GrangerOK:       true,
		GrangerPValue:   0,
		EffectOK:        true,
		EffectPValue:    0,
	})
	if math.Abs(cs-1) > 1e-9 {
		t.Errorf("causal_strength=%v want ~1 (세 신호 모두 최대)", cs)
	}
}

// TestComputeCausalStrength_PearsonOnly 는 Granger / effect 가 산정 skip (OK=false) 일 때 Pearson 항만
// 기여해 인과강도가 가중치 * Score 인지 검증한다. 증거 부재가 점수를 낮추는 설계의 회귀 가드다.
func TestComputeCausalStrength_PearsonOnly(t *testing.T) {
	cs := ComputeCausalStrength(CausalFactors{
		PearsonStrength: 0.8,
		GrangerOK:       false,
		EffectOK:        false,
	})
	want := CausalWeightPearson * 0.8
	if math.Abs(cs-want) > 1e-9 {
		t.Errorf("causal_strength=%v want %v (Pearson 단독 기여)", cs, want)
	}
}

// TestComputeCausalStrength_SignificanceInverts 는 유의성 항이 p-value 를 1-p 로 뒤집어 "유의할수록 큰
// 기여" 를 하는지 검증한다. 동일 Pearson 에서 Granger p-value 가 낮을수록 인과강도가 높아야 한다.
func TestComputeCausalStrength_SignificanceInverts(t *testing.T) {
	base := CausalFactors{PearsonStrength: 0.5, GrangerOK: true, EffectOK: false}
	low := base
	low.GrangerPValue = 0.01
	high := base
	high.GrangerPValue = 0.5
	if ComputeCausalStrength(low) <= ComputeCausalStrength(high) {
		t.Errorf("낮은 p-value (%v) 가 높은 p-value (%v) 보다 인과강도가 높아야 한다",
			ComputeCausalStrength(low), ComputeCausalStrength(high))
	}
}

// TestComputeCausalStrength_WeightsSumToOne 은 가중치 합이 1 임을 단언해, 세 항이 [0,1] 일 때 결과가
// [0,1] 에 머무는 산정식 불변식을 회귀 가드한다.
func TestComputeCausalStrength_WeightsSumToOne(t *testing.T) {
	sum := CausalWeightPearson + CausalWeightGranger + CausalWeightEffect
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("가중치 합=%v want 1.0 (각 항 [0,1] 시 결과 [0,1] 보장 불변식)", sum)
	}
}

// TestSelectTopN_AttachesCausalStrength 는 SelectTopN 이 각 NoisyNeighbor 에 ComputeCausalStrength
// 결과를 부착하고, 동시에 기존 개별 필드 (Score / PValue / ImpactPValue) 가 그대로 보존되는지 (#176
// 수용 조건 2 회귀 가드) 검증한다.
func TestSelectTopN_AttachesCausalStrength(t *testing.T) {
	r := makeResult("ns", "sus", "us", "pod:cpu_throttle_score:5m", "ns", "vic", "uv", latencyMetric, 0.9, 1, StatusOK)
	r.GrangerOK = true
	r.PValue = 0.02
	r.ImpactPValueOK = true
	r.ImpactPValue = 0.04
	out := SelectTopN([]CorrelationResult{r}, 10)
	if len(out) != 1 {
		t.Fatalf("len=%d want 1", len(out))
	}
	n := out[0]

	want := ComputeCausalStrength(CausalFactors{
		PearsonStrength: 0.9,
		GrangerOK:       true,
		GrangerPValue:   0.02,
		EffectOK:        true,
		EffectPValue:    0.04,
	})
	if math.Abs(n.CausalStrength-want) > 1e-9 {
		t.Errorf("CausalStrength=%v want %v", n.CausalStrength, want)
	}
	if n.CausalStrength <= 0 || n.CausalStrength > 1 {
		t.Errorf("CausalStrength=%v 가 (0,1] 밖", n.CausalStrength)
	}
	// 개별 필드 회귀 가드: 결합 지표 도입이 기존 노출을 덮어쓰지 않는다.
	if n.Score != 0.9 || n.PValue != 0.02 || n.ImpactPValue != 0.04 || !n.GrangerOK || !n.ImpactPValueOK {
		t.Errorf("개별 필드 회귀: %+v", n)
	}
}
