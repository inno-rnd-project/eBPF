package correlation

import (
	"math"
	"sort"
)

// EffectSize는 suspect 압박 구간과 비압박 구간의 victim latency 차이를 절대 영향 크기로 산정한다.
// Pearson 상관계수가 "victim latency 와 얼마나 동조하는가" 의 강도를 본다면, 본 함수는 "압박이
// victim 을 실제로 얼마나 느리게 만들었는가" 의 크기를 victim latency 와 동일 단위 (seconds) 로
// 돌려준다 (#146).
//
// 산정 방식은 차분이다. suspect 시계열의 중앙값을 임계로 high (압박) / low (비압박) 두 구간으로
// 나누고, 각 구간의 victim 값 평균 차이 (mean(victim|suspect_high) - mean(victim|suspect_low)) 를
// effect size 로 둔다. 회귀 기울기 대신 차분을 쓰는 이유는 suspect score 가 0-1 정규화 값이라
// 기울기의 단위 해석이 모호한 반면, 차분은 "압박 시 vs 비압박 시 victim latency 차이" 로 운영자가
// 직관적으로 읽을 수 있기 때문이다.
//
// 가드 규칙은 다음과 같다.
//   - NaN / Inf 가 한쪽이라도 있는 timestamp 는 pairwise 제거하고 length mismatch 는 짧은 쪽으로
//     truncate 한다 (Pearson 과 동일 정책)
//   - high / low 각 구간의 표본이 minSamples 미만이면 (0, false) 반환. suspect 가 상수라 분리가
//     안 되는 경우 (모든 값이 중앙값 이하) 도 한 구간이 비어 자연히 가드에 걸린다
//   - 차이가 0 이하면 (압박이 latency 를 줄이거나 영향이 없는 비-간섭 케이스) (0, false) 를 반환한다.
//     음의 effect size 는 간섭 영향이 아니므로 collector 가 emit 하지 않게 해 0-value noise 시리즈를
//     방지한다
//   - 양의 차이로 정상 산출 시 (impactSeconds, true) 반환
func EffectSize(suspect, victim []float64, minSamples int) (float64, bool) {
	// minSamples 가 1 미만이면 high / low 구간 표본 가드가 무력화되어 빈 구간의 0 division 이나
	// medianOf 의 빈 슬라이스 접근으로 NaN / panic 이 전파될 수 있다. exported API 방어로 즉시 skip 한다.
	if minSamples < 1 {
		return 0, false
	}

	n := len(suspect)
	if len(victim) < n {
		n = len(victim)
	}

	xs := make([]float64, 0, n)
	ys := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		x := suspect[i]
		y := victim[i]
		if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
			continue
		}
		xs = append(xs, x)
		ys = append(ys, y)
	}

	if len(xs) < minSamples*2 {
		// high / low 각각 minSamples 를 채우려면 최소 2*minSamples 표본이 필요하다.
		return 0, false
	}

	median := medianOf(xs)

	var highSum, lowSum float64
	var highCount, lowCount int
	for i, x := range xs {
		if x > median {
			highSum += ys[i]
			highCount++
		} else {
			lowSum += ys[i]
			lowCount++
		}
	}

	if highCount < minSamples || lowCount < minSamples {
		return 0, false
	}

	diff := highSum/float64(highCount) - lowSum/float64(lowCount)
	if diff <= 0 {
		// 압박 구간이 비압박 구간보다 빠르거나 (음의 차이) 차이가 없는 (0) 비-간섭 케이스는 false 로
		// 두어 collector 가 impact 시리즈를 emit 하지 않게 한다. 0-value noise 시리즈를 방지한다.
		return 0, false
	}
	return diff, true
}

// medianOf는 입력 슬라이스의 중앙값을 반환한다. 원본 순서를 보존하기 위해 복사본을 정렬한다.
// 빈 슬라이스는 호출 측 (EffectSize) 의 minSamples 가드로 도달하지 않는다.
func medianOf(values []float64) float64 {
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}
