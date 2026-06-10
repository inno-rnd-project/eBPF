// Package sources 의 confidence.go 는 #122 의 multi-source cross-reference confidence score
// 산출 알고리즘을 모은다. correlation snapshot 신호 와 netobs drop flow 신호 와 gpuobs GPU
// signal 의 가중치 합산 으로 0-1 정규화 점수 를 만든다.
package sources

// WeightCorrelation 은 multi-source confidence 산출 의 correlation source 가중치 다. correlation-
// exporter 의 Pearson score 가 가장 강한 quantitative 신호 라 0.5 로 두어 가장 큰 비중 을 부여
// 한다. 단일 correlation 신호 만 으로 confidence 0.5 (false positive guard threshold 0.3 통과)
// 가 보장 된다.
const WeightCorrelation = 0.5

// WeightNetobs 는 netobs drop flow 신호 가중치 다. 5-tuple drop rate 는 root cause indicator
// 보다 symptom 신호 에 가까워 보조 가중치 (0.3) 로 두어 correlation 보다 낮게 둔다.
const WeightNetobs = 0.3

// WeightGpuobs 는 gpuobs GPU signal 가중치 다. GPU dominant cause weight 는 4 dimension 의
// normalize 된 분포 라 cross-reference 시 보조 신호 (0.2) 로 활용 한다. 세 가중치 합산 이 1.0
// 이라 모든 source 가 최대 신호 (1.0) 일 때 confidence 도 1.0 에 도달 한다.
const WeightGpuobs = 0.2

// ConfidenceFactors 는 ComputeConfidenceScore 의 입력 인자 묶음 이다. 각 factor 는 0-1 범위
// 정규화 값 이며 매칭 신호 부재 시 0 으로 둔다. correlation factor 는 noisy neighbor max score,
// netobs factor 는 drop flow rate 의 정규화 값, gpuobs factor 는 GPU signal weight.
type ConfidenceFactors struct {
	Correlation float64
	Netobs      float64
	Gpuobs      float64
}

// ComputeConfidenceScore 는 세 source factor 의 가중치 합산 을 0-1 범위 로 반환 한다. 각 factor
// 가 1.0 초과 인 경우 clamp 후 합산 하여 confidence 가 1.0 을 넘지 않도록 강제 한다. 각 factor
// 음수 인 경우 0 으로 clamp 한다 (예: 향후 signed score 도입 시 회귀 차단).
func ComputeConfidenceScore(f ConfidenceFactors) float64 {
	c := clamp01(f.Correlation) * WeightCorrelation
	n := clamp01(f.Netobs) * WeightNetobs
	g := clamp01(f.Gpuobs) * WeightGpuobs
	return c + n + g
}

// clamp01 은 입력 값을 0-1 범위 로 자르는 헬퍼 다. 음수 는 0 으로 1.0 초과 는 1.0 으로 정규화
// 한다. ComputeConfidenceScore 의 각 factor clamp 에서 일관 사용 된다.
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
