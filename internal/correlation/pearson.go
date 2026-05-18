package correlation

import (
	"math"
)

// Pearson은 두 시계열의 lag 0 피어슨 상관계수를 산출한다. 산출 결과는 -1 에서 1 사이의 값이며 양
// 끝에서 강한 양 / 음 상관, 0 부근에서 무상관을 의미한다.
//
// 본 함수의 가드 규칙은 다음과 같다.
//   - NaN / Inf 가 한쪽이라도 있는 timestamp 는 pairwise 로 제거한다 (Prometheus 가 가끔 NaN 을
//     emit 하는 케이스 대응)
//   - length mismatch 가 있으면 짧은 쪽 기준으로 truncate 해 동일 길이로 맞춘다 (Prometheus 가
//     start/end 정렬돼도 query 응답이 부분적으로 비는 케이스 대응)
//   - 유효 표본 수가 minSamples 미만이면 (0, StatusSkippedLowSamples) 반환
//   - 두 시계열 중 하나라도 분산이 0 (모든 샘플이 같은 값) 이면 Pearson 정의상 분모가 0이라 NaN 이
//     되는데 본 함수는 (0, StatusSkippedConstant) 로 명시적 fallback 한다
//   - 정상 산출 시 (value, StatusOK) 반환
//
// 입력은 sample 의 Value 만 사용한다 (timestamp 정렬은 호출자가 보장). nil / empty 입력은 length
// 0 으로 취급되어 minSamples 가드에 걸린다.
func Pearson(a, b TimeSeries, minSamples int) (float64, Status) {
	// 짧은 쪽 길이를 기준으로 잡아 length mismatch 가 있어도 같은 길이로 맞춘다.
	n := len(a.Samples)
	if len(b.Samples) < n {
		n = len(b.Samples)
	}

	// NaN / Inf 가 어느 한쪽이라도 있는 timestamp 는 pairwise 제거. 결과는 sanitized 두 슬라이스.
	xs := make([]float64, 0, n)
	ys := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		x := a.Samples[i].Value
		y := b.Samples[i].Value
		if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
			continue
		}
		xs = append(xs, x)
		ys = append(ys, y)
	}

	if len(xs) < minSamples {
		return 0, StatusSkippedLowSamples
	}

	// 평균.
	var sumX, sumY float64
	for i := range xs {
		sumX += xs[i]
		sumY += ys[i]
	}
	count := float64(len(xs))
	meanX := sumX / count
	meanY := sumY / count

	// 분자 (공분산 ×N) 와 분모 (각 표본 표준편차 제곱합) 한 번에 누적.
	var num, denomX, denomY float64
	for i := range xs {
		dx := xs[i] - meanX
		dy := ys[i] - meanY
		num += dx * dy
		denomX += dx * dx
		denomY += dy * dy
	}

	// 분산 0 가드. 두 시계열 중 하나라도 상수면 Pearson 분모 = 0 이라 NaN. 명시적 0 fallback.
	if denomX == 0 || denomY == 0 {
		return 0, StatusSkippedConstant
	}

	corr := num / math.Sqrt(denomX*denomY)
	return corr, StatusOK
}

// PearsonWithLag 는 lag 시점 여러 개에 대해 Pearson 을 동시 산출하고 최대 절대값을 채택한다.
// lag k 는 cross-correlation 관례에 따라 "a 가 b 를 k step 앞선다" 를 뜻한다. 즉 corr(a[t], b[t+k])
// 를 산출하며 k 가 양수면 a 의 변동이 b 에 k 시점 뒤 나타나는 propagation 관계 (예: CPU 부하 발생
// 후 GPU latency 가 30s 뒤 올라가면 lag=+1 에서 최대 상관) 를 가리킨다.
//
// shift 결과 길이가 minSamples 미만이 되는 lag 는 산출에서 제외한다. 일부 lag 만 산출 가능한 경우
// status 는 StatusPartial 이며 산출 가능한 lag 중에서만 최대 절대값을 채택한다.
//
// 모든 lag 가 산출 불가면 SampleCount=0, MaxAbsValue=0, status=StatusSkippedLowSamples 를 반환한다.
// 모든 lag 가 분산 0 으로 막히면 status=StatusSkippedConstant 다.
func PearsonWithLag(a, b TimeSeries, lagSteps []int, minSamples int) CorrelationResult {
	result := CorrelationResult{
		CorrelationByLag: make(map[int]float64),
	}

	var maxAbs float64
	var maxAbsLag int
	var anyOK, anyConstant, anyLowSamples bool
	var sampleCount int

	for _, lag := range lagSteps {
		shiftedA, shiftedB := applyLag(a, b, lag)
		corr, status := Pearson(shiftedA, shiftedB, minSamples)

		switch status {
		case StatusOK:
			anyOK = true
			result.CorrelationByLag[lag] = corr
			if absVal := math.Abs(corr); absVal > maxAbs {
				maxAbs = absVal
				maxAbsLag = lag
			}
			if cur := len(shiftedA.Samples); cur < len(shiftedB.Samples) {
				if cur > sampleCount {
					sampleCount = cur
				}
			} else if len(shiftedB.Samples) > sampleCount {
				sampleCount = len(shiftedB.Samples)
			}
		case StatusSkippedConstant:
			anyConstant = true
		case StatusSkippedLowSamples:
			anyLowSamples = true
		}
	}

	result.MaxAbsLag = maxAbsLag
	result.MaxAbsValue = maxAbs
	result.SampleCount = sampleCount

	switch {
	case anyOK && (anyConstant || anyLowSamples):
		result.Status = StatusPartial
	case anyOK:
		result.Status = StatusOK
	case anyConstant:
		result.Status = StatusSkippedConstant
	default:
		result.Status = StatusSkippedLowSamples
	}
	return result
}

// applyLag 는 두 시계열을 lag step 만큼 shift 해 정렬된 페어를 만든다. cross-correlation 관례에
// 따라 lag k > 0 은 corr(a[t], b[t+k]) 를 계산하므로 a 의 뒤쪽 k 개를, b 의 앞쪽 k 개를 잘라 동일
// 길이의 (a[0..n-k-1], b[k..n-1]) 페어를 만든다. lag k < 0 은 반대 방향이다. 결과 시계열의 Labels
// 는 입력을 그대로 보존한다.
func applyLag(a, b TimeSeries, lag int) (TimeSeries, TimeSeries) {
	if lag == 0 {
		return a, b
	}
	la, lb := len(a.Samples), len(b.Samples)
	n := la
	if lb < n {
		n = lb
	}
	absLag := lag
	if absLag < 0 {
		absLag = -absLag
	}
	if absLag >= n {
		return TimeSeries{Labels: a.Labels}, TimeSeries{Labels: b.Labels}
	}

	if lag > 0 {
		return TimeSeries{
				Labels:  a.Labels,
				Samples: a.Samples[:n-lag],
			}, TimeSeries{
				Labels:  b.Labels,
				Samples: b.Samples[lag:n],
			}
	}
	return TimeSeries{
			Labels:  a.Labels,
			Samples: a.Samples[-lag : n],
		}, TimeSeries{
			Labels:  b.Labels,
			Samples: b.Samples[:n+lag],
		}
}
