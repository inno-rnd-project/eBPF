package correlation

import (
	"context"
	"errors"
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
	results, err := New(fetcher, cfg).Correlate(context.Background())
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
	results, err := New(fetcher, cfg).Correlate(context.Background())
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
	results, err := New(fetcher, cfg).Correlate(context.Background())
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
	_, err := New(fetcher, cfg).Correlate(context.Background())
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
	results, err := New(fetcher, cfg).Correlate(context.Background())
	if err != nil {
		t.Fatalf("err=%v want nil (one success with empty result + one failure must NOT be classified as all-failed)", err)
	}
	if len(results) != 0 {
		t.Errorf("results=%d want 0 (no series → no pairs)", len(results))
	}
}

// TestDefaultConfigContainsCorrelationInputs 는 default config 가 본 시리즈의 신규 cause score 와
// latency / node 메트릭을 포함하는지 검증한다. zero-config 운영의 기반이다.
func TestDefaultConfigContainsCorrelationInputs(t *testing.T) {
	cfg := DefaultConfig()
	required := []string{
		"pod:cpu_throttle_score:5m",
		"pod:memory_pressure_score:5m",
		"pod:host_compute_stall_score:5m",
		"node:gpu_idle:5m",
	}
	seen := make(map[string]bool, len(cfg.DefaultMetrics))
	for _, m := range cfg.DefaultMetrics {
		seen[m] = true
	}
	for _, r := range required {
		if !seen[r] {
			t.Errorf("DefaultMetrics missing %q", r)
		}
	}
	if cfg.Window <= 0 || cfg.Step <= 0 || cfg.MinSamples <= 0 {
		t.Errorf("default window/step/minSamples must be positive, got %+v", cfg)
	}
}
