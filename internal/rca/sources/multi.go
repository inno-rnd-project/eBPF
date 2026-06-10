// Package sources 의 multi.go 는 #122 의 multi-source cross-reference 산출 시 source 별 raw
// 결과 를 0-1 정규화 factor 로 변환 하는 helper 함수 와 분모 상수 를 모은다. mapping 들이
// Sources.EvaluateConfidence 를 호출 하면 본 helper 들이 내부 적 으로 사용 된다.
package sources

import (
	"netobs/internal/rca/registry"
)

// maxNeighborScore 는 NeighborInfo 슬라이스 의 max score 를 0-1 범위 로 돌려준다. Pearson 상관
// 계수 가 이미 0-1 범위 라 별도 정규화 불요. 빈 슬라이스 는 0 을 돌려준다.
func maxNeighborScore(neighbors []registry.NeighborInfo) float64 {
	max := 0.0
	for _, n := range neighbors {
		s := n.Score
		if s < 0 {
			s = -s
		}
		if s > max {
			max = s
		}
	}
	return max
}

// DropFlowNormalizationRate 는 netobs drop flow rate 의 0-1 정규화 분모 다. 운영 환경 의
// burst 임계 (per-flow drop rate 100/sec) 를 기준 으로 두어 100/sec 이상 의 burst 는 1.0 으로
// clamp 된다. ConfidenceFactors.Netobs 가 본 정규화 결과를 그대로 받아 weight 합산 에 사용
// 한다.
const DropFlowNormalizationRate = 100.0

// maxDropFlowFactor 는 DropFlowInfo 슬라이스 의 max rate 를 DropFlowNormalizationRate 로 나눠
// 0-1 범위 의 정규화 값을 돌려준다. 빈 슬라이스 는 0 을 돌려준다. clamp01 을 함수 단독 출력 에
// 도 적용 해 factor 자체 의 의미 (0-1 범위) 가 ComputeConfidenceScore 호출 외 의 재사용 경로 에
// 서도 보존 되도록 한다.
func maxDropFlowFactor(flows []registry.DropFlowInfo) float64 {
	max := 0.0
	for _, f := range flows {
		if f.RatePerSec > max {
			max = f.RatePerSec
		}
	}
	if max <= 0 {
		return 0
	}
	return clamp01(max / DropFlowNormalizationRate)
}
