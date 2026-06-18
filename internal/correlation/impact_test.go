package correlation

import (
	"math"
	"testing"
)

// TestEffectSize_BasicDiff 는 suspect 압박 구간의 victim latency 가 비압박 구간보다 명확히 높을 때
// 그 차이를 effect size 로 산정하는지 검증한다. suspect 가 high (0.8, 0.9, 1.0) 일 때 victim 이
// 0.10s 대, low (0.0, 0.1, 0.2) 일 때 0.01s 대면 effect size 는 약 0.09s 다.
func TestEffectSize_BasicDiff(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim := []float64{0.01, 0.01, 0.01, 0.10, 0.10, 0.10}

	got, ok := EffectSize(suspect, victim, 3)
	if !ok {
		t.Fatalf("EffectSize ok=false want true")
	}
	if math.Abs(got-0.09) > 1e-9 {
		t.Errorf("EffectSize=%v want ~0.09", got)
	}
}

// TestEffectSize_NegativeClamp 는 압박 구간 victim 이 오히려 낮은 (음의 차이) 케이스가 0 으로 clamp
// 되는지 검증한다. 음의 effect size 는 간섭 영향이 아니므로 노출하지 않는다.
func TestEffectSize_NegativeClamp(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim := []float64{0.10, 0.10, 0.10, 0.01, 0.01, 0.01}

	got, ok := EffectSize(suspect, victim, 3)
	if !ok {
		t.Fatalf("EffectSize ok=false want true")
	}
	if got != 0 {
		t.Errorf("EffectSize=%v want 0 (음의 차이 clamp)", got)
	}
}

// TestEffectSize_LowSamples 는 표본이 high/low 각 minSamples 를 못 채우면 (0, false) 인지 검증한다.
func TestEffectSize_LowSamples(t *testing.T) {
	suspect := []float64{0.0, 1.0, 0.5}
	victim := []float64{0.01, 0.10, 0.05}

	if got, ok := EffectSize(suspect, victim, 3); ok {
		t.Errorf("EffectSize=(%v,true) want (_,false) (표본 부족)", got)
	}
}

// TestEffectSize_ConstantSuspect 는 suspect 가 상수라 high/low 분리가 안 될 때 (0, false) 인지
// 검증한다. 모든 값이 중앙값 이하라 high 구간이 비어 가드에 걸린다.
func TestEffectSize_ConstantSuspect(t *testing.T) {
	suspect := []float64{0.5, 0.5, 0.5, 0.5, 0.5, 0.5}
	victim := []float64{0.01, 0.02, 0.03, 0.04, 0.05, 0.06}

	if got, ok := EffectSize(suspect, victim, 3); ok {
		t.Errorf("EffectSize=(%v,true) want (_,false) (상수 suspect 분리 불가)", got)
	}
}

// TestEffectSize_NaNPairwiseDrop 는 NaN 이 포함된 timestamp 가 pairwise 제거된 뒤 산정되는지
// 검증한다. NaN 제거 후 high/low 각 minSamples 를 채우면 정상 산출한다.
func TestEffectSize_NaNPairwiseDrop(t *testing.T) {
	nan := math.NaN()
	suspect := []float64{nan, 0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim := []float64{0.99, 0.01, 0.01, 0.01, 0.10, 0.10, 0.10}

	got, ok := EffectSize(suspect, victim, 3)
	if !ok {
		t.Fatalf("EffectSize ok=false want true (NaN pairwise 제거 후 충분)")
	}
	if math.Abs(got-0.09) > 1e-9 {
		t.Errorf("EffectSize=%v want ~0.09 (NaN 샘플 제외)", got)
	}
}

// TestMedianOf 는 홀수 / 짝수 길이의 중앙값 산정과 원본 순서 보존을 검증한다.
func TestMedianOf(t *testing.T) {
	odd := []float64{3, 1, 2}
	if m := medianOf(odd); m != 2 {
		t.Errorf("medianOf(odd)=%v want 2", m)
	}
	// 원본 순서 보존 (정렬 부작용 없음).
	if odd[0] != 3 || odd[1] != 1 || odd[2] != 2 {
		t.Errorf("medianOf가 입력 슬라이스를 변형함: %v", odd)
	}
	even := []float64{4, 1, 3, 2}
	if m := medianOf(even); m != 2.5 {
		t.Errorf("medianOf(even)=%v want 2.5", m)
	}
}
