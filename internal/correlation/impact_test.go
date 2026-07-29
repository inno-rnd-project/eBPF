package correlation

import (
	"math"
	"testing"
)

// TestEffectSize_BasicDiff 는 latency victim 의 압박 구간 값이 비압박 구간보다 명확히 높을 때 그 차이를
// effect size magnitude 로 산정하는지 검증한다. 두 구간이 각각 상수라 분산이 0 이므로 유의성 (PValue)
// 은 graceful skip 되고 magnitude 만 산출된다.
func TestEffectSize_BasicDiff(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim := []float64{0.01, 0.01, 0.01, 0.10, 0.10, 0.10}

	res := EffectSize(suspect, victim, SignalLatency, 3)
	if !res.OK {
		t.Fatalf("EffectSize OK=false want true")
	}
	if math.Abs(res.Magnitude-0.09) > 1e-9 {
		t.Errorf("Magnitude=%v want ~0.09", res.Magnitude)
	}
	if res.PValueOK {
		t.Errorf("PValueOK=true want false (두 구간 분산 0 → 유의성 graceful skip)")
	}
}

// TestEffectSize_NegativeSkip 은 latency 압박 구간 값이 오히려 낮은 (음의 차이) 케이스가 skip 되는지
// 검증한다. 음의 effect size 는 간섭 영향이 아니므로 collector 가 emit 하지 않도록 OK=false 로 둔다.
func TestEffectSize_NegativeSkip(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim := []float64{0.10, 0.10, 0.10, 0.01, 0.01, 0.01}

	res := EffectSize(suspect, victim, SignalLatency, 3)
	if res.OK {
		t.Fatalf("OK=true want false (음의 차이는 skip)")
	}
	if res.Magnitude != 0 || res.PValueOK {
		t.Errorf("res=%+v want zero (skip)", res)
	}
}

// TestEffectSize_ThroughputDirection 은 #175 의 수용 조건이다. throughput victim 은 압박 시 값이 감소
// 하므로 degradation 방향이 latency 와 반대다. 압박 구간 throughput 이 비압박보다 낮으면 그 감소량이
// 양의 magnitude 로 산출되는지 검증한다 (latency 방향이면 음수라 skip 됐을 케이스).
func TestEffectSize_ThroughputDirection(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	// 압박 (high) 구간 throughput 이 낮고 비압박 (low) 이 높다.
	victim := []float64{100, 100, 100, 40, 40, 40}

	res := EffectSize(suspect, victim, SignalThroughput, 3)
	if !res.OK {
		t.Fatalf("throughput OK=false want true (감소량 60 이 양의 magnitude)")
	}
	if math.Abs(res.Magnitude-60) > 1e-9 {
		t.Errorf("Magnitude=%v want 60 (low-high 감소량)", res.Magnitude)
	}
	// latency 방향이었다면 high-low = -60 이라 skip 됐어야 한다 (방향 인식 회귀 가드).
	if lat := EffectSize(suspect, victim, SignalLatency, 3); lat.OK {
		t.Errorf("동일 데이터의 latency 방향 OK=true want false (감소는 latency degradation 아님)")
	}
}

// TestEffectSize_ErrorAndGPUDirection 은 error (증가형) 와 gpu (감소형) victim 의 영향 크기가 각 방향
// 으로 산출되는지 검증한다. #175 의 throughput/error 확장과 #174 gpu 의 일관 처리를 함께 가드한다.
func TestEffectSize_ErrorAndGPUDirection(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}

	// error 는 압박 시 증가 (latency 와 동일 방향).
	errVictim := []float64{1, 1, 1, 5, 5, 5}
	if res := EffectSize(suspect, errVictim, SignalError, 3); !res.OK || math.Abs(res.Magnitude-4) > 1e-9 {
		t.Errorf("error EffectSize=%+v want OK/Magnitude~4", res)
	}

	// gpu 사용률은 압박 시 감소 (throughput 과 동일 방향).
	gpuVictim := []float64{90, 90, 90, 30, 30, 30}
	if res := EffectSize(suspect, gpuVictim, SignalGPU, 3); !res.OK || math.Abs(res.Magnitude-60) > 1e-9 {
		t.Errorf("gpu EffectSize=%+v want OK/Magnitude~60", res)
	}
}

// TestEffectSize_PValueSignificant 는 두 구간이 분산을 가지면서 평균이 뚜렷이 갈릴 때 Welch t-test
// p-value 가 산출되고 (PValueOK=true) 유의 (p < 0.05) 한지 검증한다.
func TestEffectSize_PValueSignificant(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.3, 0.7, 0.8, 0.9, 1.0}
	victim := []float64{0.010, 0.012, 0.011, 0.013, 0.100, 0.105, 0.098, 0.102}

	res := EffectSize(suspect, victim, SignalLatency, 4)
	if !res.OK {
		t.Fatalf("OK=false want true")
	}
	if !res.PValueOK {
		t.Fatalf("PValueOK=false want true (구간 분산 존재)")
	}
	if res.PValue >= 0.05 {
		t.Errorf("PValue=%v want < 0.05 (뚜렷한 평균 분리)", res.PValue)
	}
}

// TestEffectSize_PValueGracefulSkip 은 #175 의 핵심 수용 조건이다. 표본이 high/low 각 구간을 못 채우면
// magnitude 와 유의성이 모두 graceful skip (OK=false, PValueOK=false) 되어 panic 없이 빈 결과가
// 돌아오는지 검증한다.
func TestEffectSize_PValueGracefulSkip(t *testing.T) {
	// 전체 표본이 2*minSamples 미만이라 구간 분리 전에 skip.
	suspect := []float64{0.0, 1.0, 0.5}
	victim := []float64{0.01, 0.10, 0.05}
	if res := EffectSize(suspect, victim, SignalLatency, 3); res.OK || res.PValueOK {
		t.Errorf("표본 부족 res=%+v want OK=false/PValueOK=false", res)
	}

	// 압박 구간 degradation 은 있으나 (magnitude>0) 두 구간 모두 상수라 분산 0 → 유의성만 graceful skip.
	suspect2 := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim2 := []float64{0.01, 0.01, 0.01, 0.10, 0.10, 0.10}
	res2 := EffectSize(suspect2, victim2, SignalLatency, 3)
	if !res2.OK {
		t.Fatalf("res2 OK=false want true (magnitude 0.09 존재)")
	}
	if res2.PValueOK || res2.PValue != 0 {
		t.Errorf("res2 PValue=%v PValueOK=%v want 0/false (분산 0 → 유의성 skip)", res2.PValue, res2.PValueOK)
	}
}

// TestEffectSize_GuardMinSamples 는 minSamples 가 1 미만이면 즉시 skip 하는지 검증한다. 0 division /
// panic 전파를 막는 exported API 가드의 회귀 가드다.
func TestEffectSize_GuardMinSamples(t *testing.T) {
	suspect := []float64{0.0, 1.0}
	victim := []float64{0.01, 0.10}
	if res := EffectSize(suspect, victim, SignalLatency, 0); res.OK {
		t.Errorf("EffectSize(minSamples=0) OK=true want false")
	}
	if res := EffectSize(suspect, victim, SignalLatency, -1); res.OK {
		t.Errorf("EffectSize(minSamples=-1) OK=true want false")
	}
}

// TestEffectSize_SignalNoneSkip 은 victim 신호가 아닌 (SignalNone) 메트릭에 대해 즉시 skip 하는지
// 검증한다. correlator 가 reverse 페어 (dst 가 suspect) 에 호출해도 자연히 빈 결과가 된다.
func TestEffectSize_SignalNoneSkip(t *testing.T) {
	suspect := []float64{0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim := []float64{0.01, 0.01, 0.01, 0.10, 0.10, 0.10}
	if res := EffectSize(suspect, victim, SignalNone, 3); res.OK || res.PValueOK {
		t.Errorf("SignalNone res=%+v want skip", res)
	}
}

// TestEffectSize_ConstantSuspect 는 suspect 가 상수라 high/low 분리가 안 될 때 skip 하는지 검증한다.
// 모든 값이 중앙값 이하라 high 구간이 비어 가드에 걸린다.
func TestEffectSize_ConstantSuspect(t *testing.T) {
	suspect := []float64{0.5, 0.5, 0.5, 0.5, 0.5, 0.5}
	victim := []float64{0.01, 0.02, 0.03, 0.04, 0.05, 0.06}

	if res := EffectSize(suspect, victim, SignalLatency, 3); res.OK {
		t.Errorf("res=%+v want OK=false (상수 suspect 분리 불가)", res)
	}
}

// TestEffectSize_NaNPairwiseDrop 는 NaN 이 포함된 timestamp 가 pairwise 제거된 뒤 산정되는지 검증한다.
func TestEffectSize_NaNPairwiseDrop(t *testing.T) {
	nan := math.NaN()
	suspect := []float64{nan, 0.0, 0.1, 0.2, 0.8, 0.9, 1.0}
	victim := []float64{0.99, 0.01, 0.01, 0.01, 0.10, 0.10, 0.10}

	res := EffectSize(suspect, victim, SignalLatency, 3)
	if !res.OK {
		t.Fatalf("OK=false want true (NaN pairwise 제거 후 충분)")
	}
	if math.Abs(res.Magnitude-0.09) > 1e-9 {
		t.Errorf("Magnitude=%v want ~0.09 (NaN 샘플 제외)", res.Magnitude)
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

// TestAlignByLag 는 alignByLag 의 cross-correlation shift 관례 (pearson.go 의 applyLag 와 동일) 와
// 경계 조건을 검증한다. lag k > 0 은 suspect[t] 를 victim[t+k] 와 짝짓고, lag 이 길이 이상이면 빈
// 슬라이스를 돌려준다 (#363).
func TestAlignByLag(t *testing.T) {
	s := []float64{0, 1, 2, 3, 4}
	v := []float64{10, 11, 12, 13, 14}

	// lag 0: 원본 그대로.
	if as, av := alignByLag(s, v, 0); len(as) != 5 || len(av) != 5 || as[0] != 0 || av[0] != 10 {
		t.Errorf("lag 0 정렬 실패: as=%v av=%v", as, av)
	}
	// lag 2 (suspect 선행): suspect[:3]=[0,1,2] 와 victim[2:5]=[12,13,14] 를 짝짓는다.
	as, av := alignByLag(s, v, 2)
	if len(as) != 3 || len(av) != 3 || as[0] != 0 || as[2] != 2 || av[0] != 12 || av[2] != 14 {
		t.Errorf("lag 2 정렬 실패: as=%v av=%v want [0,1,2]/[12,13,14]", as, av)
	}
	// lag -1 (반대 방향): suspect[1:5]=[1,2,3,4] 와 victim[:4]=[10,11,12,13].
	as, av = alignByLag(s, v, -1)
	if len(as) != 4 || len(av) != 4 || as[0] != 1 || av[0] != 10 || av[3] != 13 {
		t.Errorf("lag -1 정렬 실패: as=%v av=%v", as, av)
	}
	// lag 이 길이 이상: 겹침 없음 → 빈 슬라이스.
	if as, av := alignByLag(s, v, 5); as != nil || av != nil {
		t.Errorf("lag>=n 정렬 실패: as=%v av=%v want nil/nil", as, av)
	}
}

// TestEffectSize_LagAlignmentRecoversMagnitude 는 #363 의 핵심 회귀 가드다. suspect 가 victim 을 2 step
// 선행하면 victim degradation 이 2 step 뒤에 나타나, lag 0 원계열로 high/low 를 나누면 압박 구간에
// 아직 상승 전 victim 값이 섞여 magnitude 가 희석된다. alignByLag 로 lag 2 에서 정렬하면 압박 구간이
// 실제 degradation 과 정렬돼 참 magnitude 가 복원되고 Welch p-value 도 산출된다.
func TestEffectSize_LagAlignmentRecoversMagnitude(t *testing.T) {
	// suspect: 앞 5 개 비압박(0), 뒤 5 개 압박(1). victim: 압박 영향이 2 step 뒤 (index 7~9) 에 상승.
	suspect := []float64{0, 0, 0, 0, 0, 1, 1, 1, 1, 1}
	victim := []float64{0.010, 0.012, 0.009, 0.011, 0.010, 0.011, 0.010, 0.101, 0.099, 0.100}

	// lag 0 원계열: 압박 구간 (index 5~9) victim 에 아직 상승 전 값 (index 5,6) 이 섞여 희석.
	raw := EffectSize(suspect, victim, SignalLatency, 2)
	if !raw.OK {
		t.Fatalf("raw OK=false want true")
	}

	// lag 2 정렬: 압박 구간이 실제 degradation (index 7~9) 과 정렬.
	as, av := alignByLag(suspect, victim, 2)
	aligned := EffectSize(as, av, SignalLatency, 2)
	if !aligned.OK {
		t.Fatalf("aligned OK=false want true")
	}

	// 정렬 magnitude 가 참값 (~0.09) 에 수렴하고 희석된 raw 보다 뚜렷이 크다.
	if math.Abs(aligned.Magnitude-0.09) > 5e-3 {
		t.Errorf("aligned Magnitude=%v want ~0.09 (참 degradation)", aligned.Magnitude)
	}
	if aligned.Magnitude <= raw.Magnitude {
		t.Errorf("aligned(%v) <= raw(%v): lag 정렬이 희석을 복원하지 못함", aligned.Magnitude, raw.Magnitude)
	}
	// 정렬 후에는 압박/비압박 구간이 분산을 가져 Welch p-value 가 산출된다.
	if !aligned.PValueOK {
		t.Errorf("aligned PValueOK=false want true (정렬 후 구간 분산 존재)")
	}
}
