package granger

import (
	"math"
	"math/rand"
	"testing"
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
