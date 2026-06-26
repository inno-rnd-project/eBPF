package correlation

import "math"

// 인과강도 가중치 (#176 산정식의 단일 진실원). Pearson 강도가 co-movement 의 1차 신호라 최대 가중을
// 두고, Granger 유의성이 인과 방향 confidence 를, effect size 유의성이 영향의 실재성을 보강한다. 세
// 가중치의 합이 1.0 이고 각 입력 항이 [0,1] 이므로 가중합도 [0,1] 에 머문다. 환경별 자동 튜닝은 비목표
// 라 본 고정값을 단일 진실원으로 둔다.
const (
	CausalWeightPearson = 0.5
	CausalWeightGranger = 0.3
	CausalWeightEffect  = 0.2
)

// CausalFactors 는 ComputeCausalStrength 의 입력이다. 세 신호를 각각 [0,1] 강도 / 유의성으로 정규화해
// 전달한다. 유의성 항 (Granger / Effect) 은 p-value 가 아니라 OK 플래그와 raw p-value 를 함께 받아
// 산정 skip 케이스를 0 으로 처리한다.
type CausalFactors struct {
	// PearsonStrength 는 max|corr| (Score) 로 이미 [0,1] 범위다.
	PearsonStrength float64
	// GrangerOK 가 false 면 (표본 부족 / 행렬 singular) Granger 항을 0 으로 둔다.
	GrangerOK     bool
	GrangerPValue float64
	// EffectOK 가 false 면 (effect size 유의성 graceful skip) effect 항을 0 으로 둔다.
	EffectOK     bool
	EffectPValue float64
}

// ComputeCausalStrength 는 Pearson 강도와 Granger 유의성과 effect size 유의성을 가중합한 [0,1] 통합
// 인과강도를 돌려준다 (#176). 산정식과 가중치의 단일 진실원이며 메트릭 / API / 대시보드가 본 결과를
// 그대로 노출한다. 각 유의성 항은 p-value 를 1-p 로 뒤집어 "유의할수록 1" 로 만들고, 산정 skip
// (OK=false) 시 0 으로 둬 증거 부재가 점수를 낮추게 한다. 가중치 합의 부동소수 오차나 비정상 입력
// (NaN, 음수) 방어로 결과를 [0,1] 로 clamp 한다.
func ComputeCausalStrength(f CausalFactors) float64 {
	s := clamp01(f.PearsonStrength)
	g := 0.0
	if f.GrangerOK {
		g = clamp01(1 - f.GrangerPValue)
	}
	e := 0.0
	if f.EffectOK {
		e = clamp01(1 - f.EffectPValue)
	}
	return clamp01(CausalWeightPearson*s + CausalWeightGranger*g + CausalWeightEffect*e)
}

// clamp01 은 입력을 [0,1] 로 가둔다. NaN 과 음수는 0, 1 초과는 1 로 둔다.
func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
