package correlation

import (
	"math"
	"sort"

	"gonum.org/v1/gonum/stat/distuv"
)

// EffectSizeResult 는 EffectSize 의 산출 결과다. Magnitude 는 victim 신호별 native 단위의 degradation
// 크기 (latency=seconds 증가, throughput=bytes/s 감소, error=drops/s 증가, gpu=util 감소) 이며, OK 가
// false 면 표본 부족이나 비-간섭 (degradation <= 0) 으로 산정이 자연 skip 된 상태다. PValue 는 high /
// low 구간 평균 차이의 Welch (unequal variance) two-sample t-test two-sided p-value 이고, PValueOK 가
// false 면 구간 분산 0 이나 표본 부족으로 유의성 산정이 graceful skip 된 상태다 (#175).
type EffectSizeResult struct {
	Magnitude float64
	OK        bool
	PValue    float64
	PValueOK  bool
}

// victimDegradesUp 은 victim 신호가 값 증가로 악화되는지 (latency / error) 감소로 악화되는지
// (throughput / gpu) 를 돌려준다. EffectSize 의 degradation 방향 결정에 쓰인다. 알 수 없는 신호는
// 보수적으로 증가형으로 둔다. #175.
func victimDegradesUp(signal VictimSignal) bool {
	switch signal {
	case SignalThroughput, SignalGPU:
		return false
	default:
		return true
	}
}

// EffectSize 는 suspect 압박 구간과 비압박 구간의 victim 값 차이를 victim 신호별 native 단위의 절대
// 영향 크기로 산정하고, 그 차이의 통계적 유의성을 Welch t-test p-value 로 함께 돌려준다 (#146 / #175).
// Pearson 상관계수가 "victim 과 얼마나 동조하는가" 의 강도를 본다면, 본 함수는 "압박이 victim 품질을
// 실제로 얼마나 악화시켰는가" 의 크기와 그 크기가 우연이 아닌지의 유의성을 본다.
//
// 산정 방식은 차분이다. suspect 시계열의 중앙값을 임계로 high (압박) / low (비압박) 두 구간으로 나누고
// 각 구간의 victim 값 평균 차이를 degradation 방향 (victimDegradesUp) 에 맞춰 effect size 로 둔다. 회귀
// 기울기 대신 차분을 쓰는 이유는 suspect score 가 0-1 정규화 값이라 기울기의 단위 해석이 모호한 반면
// 차분은 "압박 시 vs 비압박 시 victim 품질 차이" 로 운영자가 직관적으로 읽을 수 있기 때문이다.
//
// 가드 규칙은 다음과 같다.
//   - signal 이 SignalNone (victim 시계열이 아님) 이면 즉시 skip 한다. correlator 가 reverse 페어
//     (dst 가 suspect) 에 본 함수를 호출해도 자연히 빈 결과가 된다
//   - NaN / Inf 가 한쪽이라도 있는 timestamp 는 pairwise 제거하고 length mismatch 는 짧은 쪽으로
//     truncate 한다 (Pearson 과 동일 정책)
//   - high / low 각 구간의 표본이 minSamples 미만이면 skip 한다. suspect 가 상수라 분리가 안 되는
//     경우 (모든 값이 중앙값 이하) 도 한 구간이 비어 자연히 가드에 걸린다
//   - degradation 방향 차이가 0 이하면 (압박이 품질을 개선하거나 영향이 없는 비-간섭 케이스) skip 한다.
//     음의 effect size 는 간섭 영향이 아니므로 collector 가 emit 하지 않게 해 0-value noise 를 방지한다
//   - 양의 degradation 으로 정상 산출 시 OK=true 와 Welch t-test p-value (PValueOK 가드 통과 시) 를
//     함께 채운다
func EffectSize(suspect, victim []float64, signal VictimSignal, minSamples int) EffectSizeResult {
	// minSamples 가 1 미만이면 high / low 구간 표본 가드가 무력화되어 빈 구간의 0 division 이나
	// medianOf 의 빈 슬라이스 접근으로 NaN / panic 이 전파될 수 있다. exported API 방어로 즉시 skip 한다.
	if minSamples < 1 || signal == SignalNone {
		return EffectSizeResult{}
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
		return EffectSizeResult{}
	}

	median := medianOf(xs)

	highVals := make([]float64, 0, len(ys))
	lowVals := make([]float64, 0, len(ys))
	for i, x := range xs {
		if x > median {
			highVals = append(highVals, ys[i])
		} else {
			lowVals = append(lowVals, ys[i])
		}
	}

	if len(highVals) < minSamples || len(lowVals) < minSamples {
		return EffectSizeResult{}
	}

	meanHigh := mean(highVals)
	meanLow := mean(lowVals)

	var magnitude float64
	if victimDegradesUp(signal) {
		magnitude = meanHigh - meanLow
	} else {
		magnitude = meanLow - meanHigh
	}
	if magnitude <= 0 {
		// 압박 구간이 비압박 구간보다 품질이 낫거나 (음의 차이) 차이가 없는 (0) 비-간섭 케이스는 skip 해
		// collector 가 impact 시리즈를 emit 하지 않게 한다. 0-value noise 시리즈를 방지한다.
		return EffectSizeResult{}
	}

	res := EffectSizeResult{Magnitude: magnitude, OK: true}
	if p, ok := welchTTest(highVals, lowVals); ok {
		res.PValue = p
		res.PValueOK = true
	}
	return res
}

// welchTTest 는 두 표본 평균 차이에 대한 Welch (unequal variance) two-sided t-test p-value 를 돌려준다.
// 한쪽 표본이 2 미만이거나 두 구간 분산이 모두 0 이거나 Welch-Satterthwaite 자유도가 비정상이면 유의성
// 산정이 불가해 graceful skip (false) 한다. t 분포 survival 함수는 gonum distuv 로 산출한다. #175.
func welchTTest(a, b []float64) (float64, bool) {
	na, nb := len(a), len(b)
	if na < 2 || nb < 2 {
		return 0, false
	}
	ma, va := meanVar(a)
	mb, vb := meanVar(b)
	sea := va / float64(na)
	seb := vb / float64(nb)
	se := sea + seb
	// 두 구간이 사실상 상수 (분산 0, 또는 양자화 / 부동소수점 합산 오차로 분산이 데이터 scale 대비
	// 무시할 수준) 면 standard error 가 0 에 수렴해 t 통계량이 발산하고 spurious 한 p~0 이 나온다. t-test
	// 가정 (구간 내 변동 존재) 이 깨진 케이스라 유의성을 graceful skip 한다. magnitude 는 별도로 산출된다.
	scale := math.Max(math.Abs(ma), math.Abs(mb))
	if se <= 0 || math.Sqrt(se) <= scale*1e-9 {
		return 0, false
	}
	t := (ma - mb) / math.Sqrt(se)
	// Welch-Satterthwaite 자유도.
	df := (se * se) / (sea*sea/float64(na-1) + seb*seb/float64(nb-1))
	if df <= 0 || math.IsNaN(df) || math.IsInf(df, 0) {
		return 0, false
	}
	dist := distuv.StudentsT{Mu: 0, Sigma: 1, Nu: df}
	p := 2 * dist.Survival(math.Abs(t))
	if math.IsNaN(p) || math.IsInf(p, 0) {
		return 0, false
	}
	if p > 1 {
		p = 1
	}
	return p, true
}

// mean 은 슬라이스 평균이다. 빈 슬라이스는 호출 측 가드로 도달하지 않는다.
func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

// meanVar 는 표본 평균과 표본 분산 (n-1 분모) 을 돌려준다. 길이 2 미만은 호출 측 (welchTTest) 가드로
// 도달하지 않는다.
func meanVar(v []float64) (float64, float64) {
	m := mean(v)
	var ss float64
	for _, x := range v {
		d := x - m
		ss += d * d
	}
	return m, ss / float64(len(v)-1)
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
