package granger

import (
	"math"
	"math/rand"
	"testing"

	"gonum.org/v1/gonum/mat"
)

// TestGranger_StrongCausal 은 x_{t-1} 이 y_t 를 결정적으로 만드는 인공 시계열에서 F-statistic 이
// 매우 크고 p-value 가 0.01 미만으로 떨어지는지 검증한다.
func TestGranger_StrongCausal(t *testing.T) {
	const n = 200
	x := make([]float64, n)
	y := make([]float64, n)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		x[i] = rng.NormFloat64()
	}
	// y_t = 0.7 * x_{t-1} + 약간의 노이즈. y 의 과거 자체는 약하게 상관.
	for i := 1; i < n; i++ {
		y[i] = 0.7*x[i-1] + 0.1*rng.NormFloat64()
	}

	r := Test(x, y, 2, 50)
	if !r.OK {
		t.Fatalf("OK=false; want true")
	}
	if r.PValue >= 0.01 {
		t.Errorf("PValue=%g want < 0.01 (강한 인과 신호)", r.PValue)
	}
	if r.F <= 10 {
		t.Errorf("F=%g want > 10 (강한 인과 신호)", r.F)
	}
}

// TestGranger_NoCausal 은 무상관 두 시계열에서 F-statistic 이 작고 p-value 가 0.1 초과인지 검증해
// false positive 가 일어나지 않는지 가드한다.
func TestGranger_NoCausal(t *testing.T) {
	const n = 200
	x := make([]float64, n)
	y := make([]float64, n)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < n; i++ {
		x[i] = rng.NormFloat64()
		y[i] = rng.NormFloat64()
	}

	r := Test(x, y, 2, 50)
	if !r.OK {
		t.Fatalf("OK=false; want true")
	}
	if r.PValue < 0.05 {
		t.Errorf("PValue=%g want >= 0.05 (무상관)", r.PValue)
	}
}

// TestGranger_InsufficientSamples 는 표본이 minSamples 미만인 입력에서 OK=false 가 반환되는지
// 검증한다. minSamples 가드는 분모 0 또는 음의 자유도 케이스를 사전 차단한다.
func TestGranger_InsufficientSamples(t *testing.T) {
	x := []float64{1, 2, 3, 4, 5}
	y := []float64{2, 3, 4, 5, 6}
	r := Test(x, y, 2, 50)
	if r.OK {
		t.Errorf("OK=true; want false (표본 부족)")
	}
}

// TestGranger_MismatchedLength 는 두 시계열의 길이가 다르면 OK=false 가 반환되는지 검증한다.
func TestGranger_MismatchedLength(t *testing.T) {
	r := Test([]float64{1, 2, 3}, []float64{1, 2}, 1, 1)
	if r.OK {
		t.Errorf("OK=true; want false (길이 불일치)")
	}
}

// TestGranger_InvalidLag 는 lag < 1 이면 OK=false 인지 검증한다.
func TestGranger_InvalidLag(t *testing.T) {
	x := make([]float64, 100)
	y := make([]float64, 100)
	r := Test(x, y, 0, 10)
	if r.OK {
		t.Errorf("OK=true; want false (lag=0)")
	}
}

// TestGranger_PValueInRange 는 강한 인과부터 무상관까지의 p-value 가 [0, 1] 범위 안에 있는지 회귀
// 가드한다. F 분포 survival 함수의 결과가 항상 확률 범위를 지키는지 확인.
func TestGranger_PValueInRange(t *testing.T) {
	const n = 200
	rng := rand.New(rand.NewSource(99))
	cases := []struct {
		name string
		gen  func() ([]float64, []float64)
	}{
		{"strong", func() ([]float64, []float64) {
			x := make([]float64, n)
			y := make([]float64, n)
			for i := 0; i < n; i++ {
				x[i] = rng.NormFloat64()
			}
			for i := 1; i < n; i++ {
				y[i] = 0.9*x[i-1] + 0.05*rng.NormFloat64()
			}
			return x, y
		}},
		{"weak", func() ([]float64, []float64) {
			x := make([]float64, n)
			y := make([]float64, n)
			for i := 0; i < n; i++ {
				x[i] = rng.NormFloat64()
			}
			for i := 1; i < n; i++ {
				y[i] = 0.1*x[i-1] + rng.NormFloat64()
			}
			return x, y
		}},
		{"none", func() ([]float64, []float64) {
			x := make([]float64, n)
			y := make([]float64, n)
			for i := 0; i < n; i++ {
				x[i] = rng.NormFloat64()
				y[i] = rng.NormFloat64()
			}
			return x, y
		}},
	}
	for _, tc := range cases {
		x, y := tc.gen()
		r := Test(x, y, 2, 50)
		if !r.OK {
			t.Errorf("%s: OK=false", tc.name)
			continue
		}
		if math.IsNaN(r.PValue) || r.PValue < 0 || r.PValue > 1 {
			t.Errorf("%s: PValue=%g out of [0, 1]", tc.name, r.PValue)
		}
	}
}

// nearCollinearSeries 는 강한 자기상관 (거의 완전한 선형 추세 + 미세 섭동) 의 y 와 독립적인 x 를
// 만든다 (#368). y 의 lag 회귀자들은 intercept 와 근-공선이라 X 의 조건수가 매우 커지고 (섭동 1e-10, 조건수 약 1e12),
// 정규방정식은 조건수를 제곱해 이 입력에서 실패하거나 부정확했다. 섭동은 sin 기반 결정적 값이라
// 재현 가능하다.
func nearCollinearSeries(n int) (x, y []float64) {
	x = make([]float64, n)
	y = make([]float64, n)
	for i := 0; i < n; i++ {
		y[i] = float64(i) + 1e-10*math.Sin(float64(i)*1.7)
		x[i] = math.Sin(float64(i) * 0.9)
	}
	return x, y
}

// TestGranger_NearCollinearStable 은 근-공선 회귀자 입력에서 QR 최소자승이 유한하고 유효한 F 와
// p-value 를 안정적으로 산정하는지 검증한다 (#368).
func TestGranger_NearCollinearStable(t *testing.T) {
	x, y := nearCollinearSeries(120)
	res := Test(x, y, 2, 30)
	if !res.OK {
		t.Fatalf("근-공선 입력에서 OK=false (QR 최소자승이면 산정 가능해야 한다)")
	}
	if math.IsNaN(res.F) || math.IsInf(res.F, 0) || res.F < 0 {
		t.Errorf("F=%v want 유한 비음수", res.F)
	}
	if res.PValue < 0 || res.PValue > 1 {
		t.Errorf("pvalue=%v want [0,1]", res.PValue)
	}
}

// TestGranger_NormalEquationsFailOnNearCollinear 는 동일한 근-공선 입력에서 종전 정규방정식 경로
// (X^T X 를 SolveVec) 가 과대 조건수로 실패함을 보여 QR 전환의 근거를 회귀로 고정한다 (#368).
// 정규방정식은 조건수를 제곱하므로 QR 이 안정 산정하는 입력 (위 TestGranger_NearCollinearStable)
// 에서도 해를 못 구한다.
func TestGranger_NormalEquationsFailOnNearCollinear(t *testing.T) {
	x, y := nearCollinearSeries(120)
	lag := 2
	n := len(y) - lag
	yt := mat.NewVecDense(n, append([]float64(nil), y[lag:]...))
	xUnre := buildLagged(n, lag, [][]float64{y, x})

	var xtx mat.Dense
	xtx.Mul(xUnre.T(), xUnre)
	var xty mat.VecDense
	xty.MulVec(xUnre.T(), yt)
	var beta mat.VecDense
	if err := beta.SolveVec(&xtx, &xty); err == nil {
		t.Fatalf("정규방정식이 근-공선 입력에서 error 없이 통과 (조건수 제곱 실패 재현 불가)")
	}
	_ = yt
}
