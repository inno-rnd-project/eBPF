// Package sources 의 multi.go 는 #122 의 multi-source cross-reference orchestration 을 모은다.
// 단일 alert 의 RCA 산출 시 correlation snapshot 과 netobs drop flow 와 gpuobs GPU signal 을
// 동시에 호출 해 ConfidenceFactors 로 묶어 ComputeConfidenceScore 에 전달 한다.
package sources

import (
	"netobs/internal/rca/registry"
)

// MultiSourceEvidence 는 Collect 결과 의 구조체 표현 이다. 각 source 의 raw 결과 와 산출 된
// confidence score 를 함께 carry 해 mapping 이 score 자체 와 source 별 신호 양쪽 을 활용 가능
// 하게 한다.
type MultiSourceEvidence struct {
	Neighbors  []registry.NeighborInfo
	DropFlows  []registry.DropFlowInfo
	GPUSignal  float64
	Confidence float64
}

// Collect 는 victim namespace 와 victim pod 와 node 를 기준 으로 세 source 를 동시 호출 후
// ConfidenceFactors 를 산출 해 ComputeConfidenceScore 의 결과 와 함께 돌려준다. 일부 source
// 가 fail 또는 empty 면 해당 factor 가 0 으로 둬져 confidence 가 자연 감쇠 된다.
//
// victim 식별 라벨 (victimNamespace 와 victimPod) 이 비어 있는 alert (예: node 단위 GPU
// thermal throttle) 에서는 correlation factor 가 자연 0 이 되고 gpuobs factor 만 유효 할 수
// 있다. mapping 이 본 함수를 직접 호출 하지 않고 부분 source 만 사용 하는 alert 도 있을 수
// 있으므로 본 함수는 모든 source 를 호출 하되 결과 의 부분 활용 은 mapping 측 에서 결정 한다.
func (s *Sources) Collect(victimNamespace, victimPod, node string) MultiSourceEvidence {
	ev := MultiSourceEvidence{}

	if victimNamespace != "" && victimPod != "" {
		ev.Neighbors = s.TopNeighbors(victimNamespace, victimPod)
		ev.DropFlows = s.TopDropFlows(victimNamespace)
	}
	if node != "" {
		ev.GPUSignal = s.gpuobs.fetchGPUSignal(node)
	}

	factors := ConfidenceFactors{
		Correlation: maxNeighborScore(ev.Neighbors),
		Netobs:      maxDropFlowFactor(ev.DropFlows),
		Gpuobs:      ev.GPUSignal,
	}
	ev.Confidence = ComputeConfidenceScore(factors)
	return ev
}

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
// 0-1 범위 의 정규화 값을 돌려준다. 빈 슬라이스 는 0 을 돌려준다.
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
	return max / DropFlowNormalizationRate
}
