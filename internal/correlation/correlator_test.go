package correlation

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// mockFetcher 는 query → 미리 등록된 LabeledSeries 슬라이스로 응답한다. 등록되지 않은 query 는
// errOnMissing 이 true 면 에러 반환, false 면 빈 슬라이스 반환.
type mockFetcher struct {
	responses     map[string][]LabeledSeries
	errors        map[string]error
	errOnMissing  bool
	defaultResult []LabeledSeries
}

func (m *mockFetcher) Fetch(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]LabeledSeries, error) {
	if err, ok := m.errors[query]; ok {
		return nil, err
	}
	if r, ok := m.responses[query]; ok {
		return r, nil
	}
	if m.errOnMissing {
		return nil, errors.New("query not registered")
	}
	return m.defaultResult, nil
}

func linearSeries(labels map[string]string, n int, base, slope float64) LabeledSeries {
	out := LabeledSeries{Series: TimeSeries{Labels: labels, Samples: make([]Sample, n)}}
	for i := 0; i < n; i++ {
		out.Series.Samples[i] = Sample{TimestampMs: int64(i) * 1000, Value: base + slope*float64(i)}
	}
	return out
}

// TestCorrelator_HappyPath 는 두 메트릭이 노드 단위 페어로 묶여 Pearson 산출까지 무사히 흘러가는지
// 검증한다. 두 시계열이 동일 선형이므로 lag 0 에서 +1 상관이 잡힌다.
func TestCorrelator_HappyPath(t *testing.T) {
	a := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p1"}, 60, 0, 1)
	a.Metric = "metric_a"
	b := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p2"}, 60, 0, 2)
	b.Metric = "metric_b"

	fetcher := &mockFetcher{
		responses: map[string][]LabeledSeries{
			"metric_a": {a},
			"metric_b": {b},
		},
	}
	cfg := Config{
		Window:         60 * time.Second,
		Step:           1 * time.Second,
		MinSamples:     5,
		LagSteps:       []int{0},
		DefaultMetrics: []string{"metric_a", "metric_b"},
	}
	results, err := New(fetcher, cfg).Correlate(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Correlate: %v", err)
	}
	// (a→b) 와 (b→a) 두 페어가 생성되어야 한다.
	if len(results) != 2 {
		t.Fatalf("results=%d want 2", len(results))
	}
	for _, r := range results {
		if r.Status != StatusOK {
			t.Errorf("pair %+v status=%q want ok", r.Pair, r.Status)
		}
		if r.MaxAbsValue < 0.99 {
			t.Errorf("pair %+v max_abs=%v want ~1.0", r.Pair, r.MaxAbsValue)
		}
	}
}

// TestCorrelator_EmptyInput 은 모든 query 가 빈 결과를 반환할 때 빈 results 와 nil error 를
// 반환하는지 검증한다.
func TestCorrelator_EmptyInput(t *testing.T) {
	fetcher := &mockFetcher{}
	cfg := Config{DefaultMetrics: []string{"any_metric"}, LagSteps: []int{0}, MinSamples: 5}
	results, err := New(fetcher, cfg).Correlate(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Correlate: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("results=%d want 0", len(results))
	}
}

// TestCorrelator_PartialFetchFailure 는 일부 query 가 실패해도 성공한 query 만으로 산출이 계속
// 되는지 검증한다. 일부 실패는 partial 한 결과를 허용하고 모든 실패만 에러로 본다.
func TestCorrelator_PartialFetchFailure(t *testing.T) {
	a := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p1"}, 60, 0, 1)
	a.Metric = "metric_a"
	b := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p2"}, 60, 0, 1)
	b.Metric = "metric_b"

	fetcher := &mockFetcher{
		responses: map[string][]LabeledSeries{
			"metric_a": {a},
			"metric_b": {b},
		},
		errors: map[string]error{
			"failing_metric": errors.New("simulated 500"),
		},
	}
	cfg := Config{
		Step: 1 * time.Second, MinSamples: 5, LagSteps: []int{0},
		DefaultMetrics: []string{"metric_a", "metric_b", "failing_metric"},
	}
	results, err := New(fetcher, cfg).Correlate(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Correlate: %v (one failure should be tolerated)", err)
	}
	if len(results) != 2 {
		t.Errorf("results=%d want 2 (only successful queries form pairs)", len(results))
	}
}

// TestCorrelator_AllFetchFailureReturnsError 는 모든 query 가 실패하면 에러로 변환되는지 검증한다.
func TestCorrelator_AllFetchFailureReturnsError(t *testing.T) {
	fetcher := &mockFetcher{
		errors: map[string]error{
			"metric_a": errors.New("simulated 500 a"),
			"metric_b": errors.New("simulated 500 b"),
		},
	}
	cfg := Config{LagSteps: []int{0}, DefaultMetrics: []string{"metric_a", "metric_b"}, MinSamples: 5}
	_, err := New(fetcher, cfg).Correlate(context.Background(), time.Now())
	if err == nil {
		t.Errorf("err=nil want non-nil when all queries fail")
	}
}

// TestCorrelator_PartialFailureWithEmptyResults 는 일부 query 가 실패하고 나머지가 빈 결과 (0
// series) 를 반환할 때 에러가 아닌 빈 결과로 처리되는지 검증한다. "all 실패" 조건은 query 수와
// 실패 수가 정확히 일치할 때만 발화한다.
func TestCorrelator_PartialFailureWithEmptyResults(t *testing.T) {
	fetcher := &mockFetcher{
		responses: map[string][]LabeledSeries{
			"metric_empty": {}, // empty result, but successful
		},
		errors: map[string]error{
			"metric_failing": errors.New("simulated 500"),
		},
	}
	cfg := Config{LagSteps: []int{0}, DefaultMetrics: []string{"metric_empty", "metric_failing"}, MinSamples: 5}
	results, err := New(fetcher, cfg).Correlate(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("err=%v want nil (one success with empty result + one failure must NOT be classified as all-failed)", err)
	}
	if len(results) != 0 {
		t.Errorf("results=%d want 0 (no series → no pairs)", len(results))
	}
}

// TestCorrelator_DeterministicEndTime 은 endTime 을 인자로 받아 fetcher 가 정확히 [endTime-Window,
// endTime] 범위로 query 되는지 검증한다. 함수가 time.Now() 에 의존하지 않아 단위 테스트와 과거
// 시점 분석이 모두 결정적임을 보장한다.
func TestCorrelator_DeterministicEndTime(t *testing.T) {
	fixed := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	window := 10 * time.Minute
	expectStart := fixed.Add(-window)

	var gotStart, gotEnd time.Time
	fetcher := &mockFetcher{}
	fetcher.responses = map[string][]LabeledSeries{}
	// Recording fetcher 로 호출 시점 캡처.
	recording := &recordingFetcher{
		inner: fetcher,
		onCall: func(start, end time.Time) {
			gotStart, gotEnd = start, end
		},
	}
	cfg := Config{Window: window, Step: 30 * time.Second, MinSamples: 5, LagSteps: []int{0}, DefaultMetrics: []string{"any"}}
	_, _ = New(recording, cfg).Correlate(context.Background(), fixed)

	if !gotEnd.Equal(fixed) {
		t.Errorf("end=%v want %v", gotEnd, fixed)
	}
	if !gotStart.Equal(expectStart) {
		t.Errorf("start=%v want %v", gotStart, expectStart)
	}
}

type recordingFetcher struct {
	inner  Fetcher
	onCall func(start, end time.Time)
}

func (r *recordingFetcher) Fetch(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]LabeledSeries, error) {
	r.onCall(start, end)
	return r.inner.Fetch(ctx, query, start, end, step)
}

// TestDefaultConfigContainsCorrelationInputs 는 default config 가 본 시리즈의 신규 pod 단위 cause
// score를 DefaultMetrics에 포함하고 #84 의 node-level pressure score를 별도 CrossNodeMetrics 필드
// 에 분리 보관하는지 검증한다. opt-in 비활성 운영 모드에서 node-level query가 fetcher 호출 셋에
// 자동 합류하지 않도록 두 리스트가 분리되어 있어야 한다.
func TestDefaultConfigContainsCorrelationInputs(t *testing.T) {
	cfg := DefaultConfig()
	requiredPod := []string{
		"pod:cpu_throttle_score:5m",
		"pod:memory_pressure_score:5m",
		"pod:host_compute_stall_score:5m",
	}
	requiredNode := []string{
		"node:cpu_pressure_score:5m",
		"node:memory_pressure_score:5m",
		"node:network_pressure_score:5m",
		"node:gpu_pressure_score:5m",
		"node:netobs_pod_stage_latency_p99:5m",
	}
	defaultSeen := make(map[string]bool, len(cfg.DefaultMetrics))
	for _, m := range cfg.DefaultMetrics {
		defaultSeen[m] = true
	}
	for _, r := range requiredPod {
		if !defaultSeen[r] {
			t.Errorf("DefaultMetrics missing pod-level metric %q", r)
		}
	}
	// node-level 메트릭은 DefaultMetrics에 포함되지 않아야 한다 (opt-in 비활성 시 fetch 회피).
	for _, m := range cfg.DefaultMetrics {
		if len(m) > 5 && m[:5] == "node:" {
			t.Errorf("DefaultMetrics 가 node-level metric %q 를 포함함 (CrossNodeMetrics 로 분리되어야 함)", m)
		}
	}
	crossSeen := make(map[string]bool, len(cfg.CrossNodeMetrics))
	for _, m := range cfg.CrossNodeMetrics {
		crossSeen[m] = true
	}
	for _, r := range requiredNode {
		if !crossSeen[r] {
			t.Errorf("CrossNodeMetrics missing node-level metric %q", r)
		}
	}
	if cfg.Window <= 0 || cfg.Step <= 0 || cfg.MinSamples <= 0 {
		t.Errorf("default window/step/minSamples must be positive, got %+v", cfg)
	}
	// #147 CrossNodeEnabled 는 default true 로 두어 zero-config 에서도 node 단위 간섭 Top-N 이 emit
	// 되는 회귀 가드. 운영자는 CROSS_NODE=false env 또는 -cross-node=false flag 로 opt-out 한다.
	if !cfg.CrossNodeEnabled {
		t.Errorf("CrossNodeEnabled default must be true for zero-config cross-node policy")
	}
	if cfg.CrossNodeMaxPairs <= 0 {
		t.Errorf("CrossNodeMaxPairs default must be positive, got %d", cfg.CrossNodeMaxPairs)
	}
}

// TestFilterWeakSuspects 는 #245 무부하 노이즈 게이트의 단위 판정을 검증한다. suspect 는 window 내
// 최대 절대값이 floor 미만이면 제거되고, victim 시계열은 native 단위라 크기와 무관하게 유지되며,
// floor 0 이하는 게이트 비활성이다.
func TestFilterWeakSuspects(t *testing.T) {
	weak := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p1"}, 10, 0, 0.001)
	weak.Metric = "pod:network_pressure_score:5m"
	strong := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p2"}, 10, 0.5, 0.01)
	strong.Metric = "pod:cpu_throttle_score:5m"
	victim := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p3"}, 10, 0, 0.0001)
	victim.Metric = "netobs_pod_stage_latency_p99"

	out := filterWeakSuspects([]LabeledSeries{weak, strong, victim}, 0.1)
	if len(out) != 2 {
		t.Fatalf("filtered=%d want 2 (weak suspect 제거, strong suspect 와 victim 유지)", len(out))
	}
	for _, it := range out {
		if it.Metric == weak.Metric {
			t.Errorf("근제로 suspect %q 가 게이트를 통과함", it.Metric)
		}
	}
	if got := filterWeakSuspects([]LabeledSeries{weak, strong, victim}, 0); len(got) != 3 {
		t.Errorf("floor=0 인데 filtered=%d want 3 (게이트 비활성)", len(got))
	}
	// NaN floor 는 env/flag 의 "NaN" 이 ParseFloat 를 무오류 통과해 도달할 수 있다. 모든 비교가
	// false 라 suspect 전체가 유실되는 대신 비활성으로 취급되어야 한다.
	if got := filterWeakSuspects([]LabeledSeries{weak, strong, victim}, math.NaN()); len(got) != 3 {
		t.Errorf("floor=NaN 인데 filtered=%d want 3 (게이트 비활성)", len(got))
	}
}

// TestCorrelator_WeakSuspectGate 는 게이트가 Correlate end-to-end 에서 근제로 suspect 의 페어 산출
// 을 차단하는지 검증한다. 무부하 노이즈 (max 0.006) suspect 는 victim 과 파형이 완전히 닮아도 결과
// 가 0 이어야 하고, 동일 구성에서 suspect 크기만 키우면 페어가 산출되어야 한다.
func TestCorrelator_WeakSuspectGate(t *testing.T) {
	mkFetcher := func(base, slope float64) *mockFetcher {
		suspect := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p1"}, 60, base, slope)
		suspect.Metric = "pod:network_pressure_score:5m"
		victim := linearSeries(map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": "p2"}, 60, 0, 0.001)
		victim.Metric = "netobs_pod_stage_latency_p99"
		return &mockFetcher{responses: map[string][]LabeledSeries{
			"pod:network_pressure_score:5m": {suspect},
			"netobs_pod_stage_latency_p99":  {victim},
		}}
	}
	cfg := Config{
		Window:          60 * time.Second,
		Step:            1 * time.Second,
		MinSamples:      5,
		LagSteps:        []int{0},
		DefaultMetrics:  []string{"pod:network_pressure_score:5m", "netobs_pod_stage_latency_p99"},
		MinSuspectScore: 0.1,
	}

	results, err := New(mkFetcher(0, 0.0001), cfg).Correlate(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Correlate(weak): %v", err)
	}
	if len(results) != 0 {
		t.Errorf("근제로 suspect 인데 results=%d want 0: %+v", len(results), results)
	}

	results, err = New(mkFetcher(0.3, 0.005), cfg).Correlate(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Correlate(strong): %v", err)
	}
	if len(results) == 0 {
		t.Errorf("유의미한 suspect 인데 results=0 (게이트 과차단)")
	}
}
