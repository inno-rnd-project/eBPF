// Package granger 는 두 시계열 사이 Granger causality 의 F-statistic 과 p-value 를 산출하는
// stateless 라이브러리다. x 가 y 를 Granger-cause 하는지 (x 의 과거 값이 y 의 현재 값을 예측하는 데
// 통계적으로 유의한 추가 정보를 제공하는지) 를 lag p 의 restricted 와 unrestricted OLS 회귀 RSS
// 차분으로 판정한다.
//
// 산정식.
//   - restricted   y_t = a0 + sum_{i=1..p} ai * y_{t-i}
//   - unrestricted y_t = a0 + sum_{i=1..p} ai * y_{t-i} + sum_{j=1..p} bj * x_{t-j}
//   - F = ((RSS_R - RSS_U) / p) / (RSS_U / (n - 2p - 1))
//   - p-value = F-distribution(df1=p, df2=n-2p-1) 의 survival 함수
//
// lag order p 는 본 라이브러리에서 호출자가 결정한다 (AIC / BIC 자동 선택은 본 패키지 scope 외).
// stationarity test (ADF, KPSS) 도 본 패키지 scope 외이며 raw 시계열을 그대로 받는다.
package granger

import (
	"math"

	"gonum.org/v1/gonum/mat"
	"gonum.org/v1/gonum/stat/distuv"
)

// Result 는 Granger test 의 출력이다. OK 가 false 면 (표본 부족, 행렬 singular 등) F 와 PValue 가
// 0 으로 발행되고 호출자는 본 결과를 자연 skip 한다.
type Result struct {
	F      float64
	PValue float64
	OK     bool
}

// Test 는 두 시계열 x, y 와 lag 로 Granger causality 를 산정한다. minSamples 는 lag 적용 후 유효
// 표본 수의 하한이다. n - lag 가 minSamples 미만이거나 분모 자유도 (n - 2*lag - 1) 가 1 미만이면
// OK=false 를 반환한다.
func Test(x, y []float64, lag, minSamples int) Result {
	if lag < 1 {
		return Result{}
	}
	if len(x) != len(y) || len(x) < lag+1 {
		return Result{}
	}
	n := len(x) - lag
	if n < minSamples {
		return Result{}
	}
	dfDenom := n - 2*lag - 1
	if dfDenom < 1 {
		return Result{}
	}

	yt := mat.NewVecDense(n, append([]float64(nil), y[lag:]...))

	xRest := buildLagged(n, lag, [][]float64{y})
	rssR, ok := ols(xRest, yt)
	if !ok {
		return Result{}
	}

	xUnre := buildLagged(n, lag, [][]float64{y, x})
	rssU, ok := ols(xUnre, yt)
	if !ok {
		return Result{}
	}

	if rssU <= 0 {
		return Result{}
	}
	if rssR < rssU {
		// 부동소수점 오차로 인한 음수 차분 가능. unrestricted 가 restricted 의 superset 이라
		// 이론적으로는 RSS_U <= RSS_R 이다.
		rssR = rssU
	}

	F := ((rssR - rssU) / float64(lag)) / (rssU / float64(dfDenom))
	if math.IsNaN(F) || math.IsInf(F, 0) {
		return Result{}
	}
	fDist := distuv.F{D1: float64(lag), D2: float64(dfDenom)}
	pvalue := fDist.Survival(F)

	return Result{F: F, PValue: pvalue, OK: true}
}

// buildLagged 는 [intercept=1, hist[0]_{t-1}, ..., hist[0]_{t-lag}, hist[1]_{t-1}, ...] 형태의 행렬을
// 만든다. histories 가 두 개면 unrestricted 모델의 X 가 되고 한 개면 restricted 모델의 X 가 된다.
func buildLagged(n, lag int, histories [][]float64) *mat.Dense {
	cols := 1 + lag*len(histories)
	data := make([]float64, n*cols)
	for i := 0; i < n; i++ {
		data[i*cols] = 1.0
		for h, hist := range histories {
			for k := 1; k <= lag; k++ {
				data[i*cols+1+h*lag+(k-1)] = hist[lag+i-k]
			}
		}
	}
	return mat.NewDense(n, cols, data)
}

// ols 는 정규 방정식 (X^T X)^-1 X^T y 로 OLS 계수를 구하고 RSS = sum((y - X*beta)^2) 반환한다.
// X^T X 가 singular 면 (특이행렬) ok=false 반환.
func ols(X *mat.Dense, y *mat.VecDense) (float64, bool) {
	var xtx mat.Dense
	xtx.Mul(X.T(), X)
	var xtxInv mat.Dense
	if err := xtxInv.Inverse(&xtx); err != nil {
		return 0, false
	}
	var xty mat.VecDense
	xty.MulVec(X.T(), y)
	var beta mat.VecDense
	beta.MulVec(&xtxInv, &xty)
	var fitted mat.VecDense
	fitted.MulVec(X, &beta)
	var resid mat.VecDense
	resid.SubVec(y, &fitted)
	rss := 0.0
	n, _ := resid.Dims()
	for i := 0; i < n; i++ {
		v := resid.AtVec(i)
		rss += v * v
	}
	if math.IsNaN(rss) || math.IsInf(rss, 0) {
		return 0, false
	}
	return rss, true
}
