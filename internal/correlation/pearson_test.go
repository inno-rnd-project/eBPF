package correlation

import (
	"math"
	"testing"
)

// samples 는 float 슬라이스로부터 timestamp 가 균등 간격인 Sample 슬라이스를 만든다. 테스트 의도가
// Pearson 산출 수치 검증이라 실제 timestamp 값은 의미가 없고 0부터 1씩 증가시킨다.
func samples(vs ...float64) []Sample {
	out := make([]Sample, len(vs))
	for i, v := range vs {
		out[i] = Sample{TimestampMs: int64(i), Value: v}
	}
	return out
}

// approxEqual 은 부동소수점 오차를 흡수한 비교다. Pearson 산출에 부동소수점 누적 오차가 끼어 들어
// 단순 == 비교는 false negative 를 만든다.
func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// TestPearsonCorrelationPlusOne 은 두 시계열이 완벽 양 상관 (y = 2x + 3) 일 때 결과가 +1 임을 검증
// 한다. Pearson 정의상 선형 변환은 +1 또는 -1 으로 떨어진다.
func TestPearsonCorrelationPlusOne(t *testing.T) {
	a := TimeSeries{Samples: samples(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)}
	b := TimeSeries{Samples: samples(5, 7, 9, 11, 13, 15, 17, 19, 21, 23)}
	got, status := Pearson(a, b, 3)
	if status != StatusOK {
		t.Fatalf("status=%q want %q", status, StatusOK)
	}
	if !approxEqual(got, 1.0, 1e-9) {
		t.Errorf("corr=%v want 1.0", got)
	}
}

// TestPearsonCorrelationMinusOne 은 완벽 음 상관 (y = -x + c) 일 때 결과가 -1 임을 검증한다.
func TestPearsonCorrelationMinusOne(t *testing.T) {
	a := TimeSeries{Samples: samples(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)}
	b := TimeSeries{Samples: samples(10, 9, 8, 7, 6, 5, 4, 3, 2, 1)}
	got, status := Pearson(a, b, 3)
	if status != StatusOK {
		t.Fatalf("status=%q want %q", status, StatusOK)
	}
	if !approxEqual(got, -1.0, 1e-9) {
		t.Errorf("corr=%v want -1.0", got)
	}
}

// TestPearsonCorrelationZero 는 두 시계열이 무상관 (한쪽 일정 / 다른쪽 변동) 이지만 상수가 아닌
// 입력에서 0에 근접한 결과를 내는지 검증한다. 완벽 0 무상관은 random sequence 에서만 일어나므로
// 본 테스트는 직교 대칭 시퀀스 (1,-1,1,-1,...) 와 (1,2,3,4,5,4,3,2,1,...) 를 사용한다.
func TestPearsonCorrelationZero(t *testing.T) {
	a := TimeSeries{Samples: samples(-1, 1, -1, 1, -1, 1, -1, 1)}
	b := TimeSeries{Samples: samples(1, 2, 3, 4, 4, 3, 2, 1)}
	got, status := Pearson(a, b, 3)
	if status != StatusOK {
		t.Fatalf("status=%q want %q", status, StatusOK)
	}
	if math.Abs(got) > 0.1 {
		t.Errorf("corr=%v want |corr| < 0.1 for orthogonal-ish sequences", got)
	}
}

// TestPearsonCorrelationModerate 는 중간 정도 상관 (예: 0.5 부근) 을 검증한다. 입력은 base sequence
// 에 일부 noise 를 더한 형태로 두 시계열이 비슷한 추세를 보이되 완벽 일치는 아니다.
func TestPearsonCorrelationModerate(t *testing.T) {
	a := TimeSeries{Samples: samples(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)}
	// b 는 a 와 같은 추세지만 매 timestamp 마다 다른 잡음을 추가해 완벽 +1 이 아닌 0.5-0.9 사이 상관
	// 을 의도적으로 만든다.
	b := TimeSeries{Samples: samples(3, 1, 4, 2, 6, 5, 9, 6, 8, 10)}
	got, status := Pearson(a, b, 3)
	if status != StatusOK {
		t.Fatalf("status=%q want %q", status, StatusOK)
	}
	if got <= 0.3 || got >= 0.99 {
		t.Errorf("corr=%v want moderate positive 0.3-0.99 (sanity for noisy ascending trend)", got)
	}
}

// TestPearsonStddevZeroReturnsConstantStatus 는 한쪽 시계열이 상수 (분산 0) 일 때 NaN 대신 0 과
// StatusSkippedConstant 를 반환하는지 검증한다. 본 가드가 빠지면 Pearson 분모 = 0 이라 NaN 이 emit
// 되어 #51 exporter 가 잘못된 메트릭을 노출한다.
func TestPearsonStddevZeroReturnsConstantStatus(t *testing.T) {
	a := TimeSeries{Samples: samples(5, 5, 5, 5, 5, 5)} // 상수
	b := TimeSeries{Samples: samples(1, 2, 3, 4, 5, 6)}
	got, status := Pearson(a, b, 3)
	if status != StatusSkippedConstant {
		t.Errorf("status=%q want %q", status, StatusSkippedConstant)
	}
	if got != 0 {
		t.Errorf("corr=%v want 0 fallback", got)
	}
	if math.IsNaN(got) {
		t.Errorf("corr is NaN; gate must replace it with 0")
	}
}

// TestPearsonLowSamplesSkipped 는 입력 표본 수가 minSamples 미만이면 산출을 skip 하고 status 로
// 표기하는지 검증한다.
func TestPearsonLowSamplesSkipped(t *testing.T) {
	a := TimeSeries{Samples: samples(1, 2)}
	b := TimeSeries{Samples: samples(3, 4)}
	got, status := Pearson(a, b, 5)
	if status != StatusSkippedLowSamples {
		t.Errorf("status=%q want %q", status, StatusSkippedLowSamples)
	}
	if got != 0 {
		t.Errorf("corr=%v want 0", got)
	}
}

// TestPearsonNilEmptyInputs 는 nil / empty 입력에서 panic 없이 low samples status 를 반환하는지
// 검증한다. 0 길이 입력은 minSamples > 0 가드에 자연스럽게 걸린다.
func TestPearsonNilEmptyInputs(t *testing.T) {
	cases := []struct {
		name string
		a, b TimeSeries
	}{
		{"both_empty", TimeSeries{}, TimeSeries{}},
		{"a_empty", TimeSeries{}, TimeSeries{Samples: samples(1, 2, 3, 4, 5)}},
		{"b_empty", TimeSeries{Samples: samples(1, 2, 3, 4, 5)}, TimeSeries{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, status := Pearson(c.a, c.b, 3)
			if status != StatusSkippedLowSamples {
				t.Errorf("status=%q want %q", status, StatusSkippedLowSamples)
			}
			if got != 0 {
				t.Errorf("corr=%v want 0", got)
			}
		})
	}
}

// TestPearsonLengthMismatchTruncates 는 두 입력의 길이가 다를 때 짧은 쪽 기준으로 truncate 해 산출
// 이 정상 수행되는지 검증한다.
func TestPearsonLengthMismatchTruncates(t *testing.T) {
	// a 는 6 개, b 는 4 개. 짧은 b 길이 4 로 truncate 되어 (1,2,3,4) 와 (1,2,3,4) 만 비교 → +1.
	a := TimeSeries{Samples: samples(1, 2, 3, 4, 99, 100)}
	b := TimeSeries{Samples: samples(1, 2, 3, 4)}
	got, status := Pearson(a, b, 3)
	if status != StatusOK {
		t.Fatalf("status=%q want %q (length mismatch should truncate, not skip)", status, StatusOK)
	}
	if !approxEqual(got, 1.0, 1e-9) {
		t.Errorf("corr=%v want 1.0 (truncated arms are perfect linear)", got)
	}
}

// TestPearsonNaNInfPairwiseRemoval 은 한쪽 시계열에 NaN / Inf 가 섞여 있을 때 해당 timestamp 만
// 제거하고 나머지로 산출하는지 검증한다. Prometheus 가 가끔 데이터 공백 timestamp 에 NaN 을 emit
// 하는 케이스에 대한 가드다.
func TestPearsonNaNInfPairwiseRemoval(t *testing.T) {
	// 두 시계열은 실제로 +1 상관이지만 a 의 2번째 sample 이 NaN, b 의 5번째가 +Inf 라 두 timestamp
	// 가 pairwise 제거되어 (1,3,4,7) 와 (1,3,4,7) 만 비교 → 여전히 +1.
	a := TimeSeries{Samples: []Sample{
		{TimestampMs: 0, Value: 1},
		{TimestampMs: 1, Value: math.NaN()},
		{TimestampMs: 2, Value: 3},
		{TimestampMs: 3, Value: 4},
		{TimestampMs: 4, Value: 5},
		{TimestampMs: 5, Value: 6},
		{TimestampMs: 6, Value: 7},
	}}
	b := TimeSeries{Samples: []Sample{
		{TimestampMs: 0, Value: 1},
		{TimestampMs: 1, Value: 2},
		{TimestampMs: 2, Value: 3},
		{TimestampMs: 3, Value: 4},
		{TimestampMs: 4, Value: math.Inf(1)},
		{TimestampMs: 5, Value: 6},
		{TimestampMs: 6, Value: 7},
	}}
	got, status := Pearson(a, b, 3)
	if status != StatusOK {
		t.Fatalf("status=%q want %q (NaN/Inf must be pairwise-removed, not poison the result)", status, StatusOK)
	}
	if !approxEqual(got, 1.0, 1e-9) {
		t.Errorf("corr=%v want 1.0 (5 clean pairs all linear)", got)
	}
}

// TestPearsonWithLagSelectsMaxAbs 는 lag 별 산출 후 최대 절대값 채택과 status 분류가 정상인지
// 검증한다. 입력은 a 가 비단조 순열이고 b 는 a 의 한 step 지연 (b[t] = a[t-1]) 이라 cross-
// correlation 관례 (lag k = corr(a[t], b[t+k])) 에서 lag=+1 일 때 a[t] = b[t+1] 이 완벽 일치해
// 최대 상관, lag 0 / -1 에서는 비단조 순열이라 약한 상관을 보인다.
func TestPearsonWithLagSelectsMaxAbs(t *testing.T) {
	a := TimeSeries{Samples: samples(1, 5, 2, 8, 3, 7, 4, 6, 9, 10)}
	// b[t] = a[t-1] (b[0] = 0 으로 채움). a 를 한 step 늦춘 형태라 a 가 b 를 한 step 앞선다.
	b := TimeSeries{Samples: samples(0, 1, 5, 2, 8, 3, 7, 4, 6, 9)}
	got := PearsonWithLag(a, b, []int{-1, 0, 1}, 3)
	if got.Status != StatusOK {
		t.Fatalf("status=%q want %q", got.Status, StatusOK)
	}
	if got.MaxAbsLag != 1 {
		t.Errorf("MaxAbsLag=%d want 1 (a leads b by 1, so corr(a[t], b[t+1]) is maximal)", got.MaxAbsLag)
	}
	if !approxEqual(got.MaxAbsValue, 1.0, 1e-9) {
		t.Errorf("MaxAbsValue=%v want ~1.0 at lag=+1", got.MaxAbsValue)
	}
}
