package selfhealth

import (
	"errors"
	"testing"
)

// fakeDropSource 는 dropSource 의 in-memory 구현이다. test 가 사이클 단위로 카운터를 조작해
// baseline-then-delta 분기 3 종 (init, reset, monotonic increase) 을 모두 가드한다.
type fakeDropSource struct{ v uint64 }

func (f *fakeDropSource) Total() uint64 { return f.v }

// fakeSizer 는 mapSizer 의 in-memory 구현이다.
type fakeSizer struct {
	name           string
	entries        uint64
	max            uint64
	err            error
	calls          int
}

func (s *fakeSizer) Name() string       { return s.name }
func (s *fakeSizer) MaxEntries() uint64 { return s.max }
func (s *fakeSizer) Entries() (uint64, error) {
	s.calls++
	return s.entries, s.err
}

// TestRefresher_DropsBaselineInit 은 첫 호출에서 baseline 만 저장되고 누적이 발생하지 않는지
// 검증한다. agent 재시작 시 BPF map 에 남아 있던 잔존 카운트로 부풀려진 spike 가 발생하지 않게
// 하는 회귀 가드다.
func TestRefresher_DropsBaselineInit(t *testing.T) {
	d := &fakeDropSource{v: 42}
	r := &Refresher{drops: d}
	var b baseline
	r.refreshOnce(&b)

	if !b.initialized {
		t.Errorf("initialized=false; want true")
	}
	if b.last != 42 {
		t.Errorf("last=%d; want 42 (baseline must match current after init)", b.last)
	}
}

// TestRefresher_DropsMonotonicIncrease 는 baseline 이후의 단조 증가에서 last 가 current 로 따라
// 가는지 검증한다. metrics 측 누적 값은 metrics 패키지 단위 테스트가 가드하고 본 자리는 baseline
// 추적 로직만 책임진다.
func TestRefresher_DropsMonotonicIncrease(t *testing.T) {
	d := &fakeDropSource{v: 10}
	r := &Refresher{drops: d}
	var b baseline
	r.refreshOnce(&b) // init at 10
	d.v = 25
	r.refreshOnce(&b)
	if b.last != 25 {
		t.Errorf("last=%d; want 25 (must track current on monotonic increase)", b.last)
	}
	d.v = 30
	r.refreshOnce(&b)
	if b.last != 30 {
		t.Errorf("last=%d; want 30", b.last)
	}
}

// TestRefresher_DropsResetSkipsAccumulation 은 current < last 인 reset 케이스에서 last 만 갱신
// 되고 metrics 누적 호출이 일어나지 않는지 검증한다. BPF map reload 시 거짓 spike 가 prometheus
// counter 로 흘러가지 않게 하는 가드다.
func TestRefresher_DropsResetSkipsAccumulation(t *testing.T) {
	d := &fakeDropSource{v: 100}
	r := &Refresher{drops: d}
	var b baseline
	r.refreshOnce(&b) // init at 100
	d.v = 5
	r.refreshOnce(&b)

	if b.last != 5 {
		t.Errorf("last=%d; want 5 (must adopt new baseline on reset)", b.last)
	}
}

// TestRefresher_MapUtilizationCallsSizer 는 sizer.Entries 가 정상 호출되는지 검증한다. 실제 gauge
// 값은 metrics 패키지의 selfhealth_test.go 가 setter 단위로 가드한다.
func TestRefresher_MapUtilizationCallsSizer(t *testing.T) {
	starts := &fakeSizer{name: "starts", entries: 4096, max: 16384}
	pb := &fakeSizer{name: "pod_bytes", entries: 8000, max: 16384}
	r := &Refresher{drops: &fakeDropSource{}, sizers: []mapSizer{starts, pb}}
	var b baseline
	r.refreshOnce(&b)

	if starts.calls != 1 {
		t.Errorf("starts.calls=%d; want 1", starts.calls)
	}
	if pb.calls != 1 {
		t.Errorf("pod_bytes.calls=%d; want 1", pb.calls)
	}
}

// TestRefresher_MapUtilizationContinuesOnError 는 sizer 에러가 다른 sizer 의 emit 을 차단하지
// 않는지 검증한다.
func TestRefresher_MapUtilizationContinuesOnError(t *testing.T) {
	starts := &fakeSizer{name: "starts", err: errors.New("iterate failed")}
	pb := &fakeSizer{name: "pod_bytes", entries: 8000, max: 16384}
	r := &Refresher{drops: &fakeDropSource{}, sizers: []mapSizer{starts, pb}}
	var b baseline
	r.refreshOnce(&b)

	if pb.calls != 1 {
		t.Errorf("pod_bytes.calls=%d; want 1 (error on starts must not block pod_bytes)", pb.calls)
	}
}

// TestRefresher_MapUtilizationZeroMaxIsNoop 은 MaxEntries=0 인 비정상 map 에서 divide-by-zero
// panic 없이 사이클이 완료되는지 검증한다. continue 후 다음 sizer 가 정상 호출되어야 한다.
func TestRefresher_MapUtilizationZeroMaxIsNoop(t *testing.T) {
	broken := &fakeSizer{name: "broken", entries: 1, max: 0}
	ok := &fakeSizer{name: "ok", entries: 100, max: 1000}
	r := &Refresher{drops: &fakeDropSource{}, sizers: []mapSizer{broken, ok}}
	var b baseline
	r.refreshOnce(&b)

	if ok.calls != 1 {
		t.Errorf("ok.calls=%d; want 1 (zero max must not stop iteration)", ok.calls)
	}
}
