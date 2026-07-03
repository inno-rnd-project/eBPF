package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"netobs/internal/correlation"
)

// fakeFetcher 는 correlation.Fetcher 테스트 더블이다. 마지막 query 와 range/step 을 기록한다.
type fakeFetcher struct {
	series   []correlation.LabeledSeries
	err      error
	gotQuery string
	gotRange time.Duration
	gotStep  time.Duration
}

func (f *fakeFetcher) Fetch(_ context.Context, query string, start, end time.Time, step time.Duration) ([]correlation.LabeledSeries, error) {
	f.gotQuery = query
	f.gotRange = end.Sub(start)
	f.gotStep = step
	if f.err != nil {
		return nil, f.err
	}
	return f.series, nil
}

func trendSeries() []correlation.LabeledSeries {
	return []correlation.LabeledSeries{{
		Metric: "max(correlation_noisy_neighbor_causal_strength)",
		Series: correlation.TimeSeries{
			Labels:  map[string]string{},
			Samples: []correlation.Sample{{TimestampMs: 1000, Value: 0.5}, {TimestampMs: 2000, Value: 0.82}},
		},
	}}
}

// TestTrends 는 화이트리스트 signal 을 range query 로 읽어 시점별 값을 돌려주고, 올바른 PromQL 을
// fetcher 에 넘기는지 검증한다.
func TestTrends(t *testing.T) {
	f := &fakeFetcher{series: trendSeries()}
	h := NewTrendsHandler(f)
	rec := httptest.NewRecorder()
	h.GetTrends(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trends?signal=noisy_neighbor_intensity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp TrendsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(f.gotQuery, "correlation_noisy_neighbor_causal_strength") {
		t.Errorf("query=%q want causal_strength 신호", f.gotQuery)
	}
	if len(resp.Series) != 1 || len(resp.Series[0].Points) != 2 || resp.Series[0].Points[1].Value != 0.82 {
		t.Errorf("series=%+v want 2 points, 마지막 0.82", resp.Series)
	}
	if !strings.Contains(resp.Summary, "noisy_neighbor_intensity") {
		t.Errorf("summary=%q", resp.Summary)
	}
}

// TestTrends_ResourceSignals 는 #214 자원 사용량 / 지연 신호 5종이 화이트리스트로 서빙되고 각각
// 올바른 PromQL 을 fetcher 에 넘기는지 검증한다.
func TestTrends_ResourceSignals(t *testing.T) {
	cases := map[string]string{
		"latency_p99":  "netobs_stage_latency_labeled_seconds_bucket",
		"drop_rate":    "netobs_drop_events_labeled_total",
		"bandwidth_rx": `direction="ingress",layer="l4"`,
		"bandwidth_tx": `direction="egress",layer="l4"`,
		"pressure_max": "node:pressure_score:5m",
	}
	for signal, want := range cases {
		f := &fakeFetcher{series: trendSeries()}
		h := NewTrendsHandler(f)
		rec := httptest.NewRecorder()
		h.GetTrends(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trends?signal="+signal, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("signal=%s status=%d want 200", signal, rec.Code)
		}
		if !strings.Contains(f.gotQuery, want) {
			t.Errorf("signal=%s query=%q want %q 포함", signal, f.gotQuery, want)
		}
	}
}

// TestTrends_RangeClamp 는 range 가 24h 상한으로, step 이 30s 하한으로 clamp 되는지 검증한다.
func TestTrends_RangeClamp(t *testing.T) {
	f := &fakeFetcher{series: trendSeries()}
	h := NewTrendsHandler(f)
	rec := httptest.NewRecorder()
	h.GetTrends(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trends?signal=cross_node_intensity&range=48h&step=1s", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if f.gotRange != 24*time.Hour {
		t.Errorf("range=%v want 24h clamp", f.gotRange)
	}
	if f.gotStep != 30*time.Second {
		t.Errorf("step=%v want 30s floor", f.gotStep)
	}
}

// TestTrends_InvalidSignal 은 화이트리스트 밖 signal 에 400 을 돌려주는지 검증한다.
func TestTrends_InvalidSignal(t *testing.T) {
	h := NewTrendsHandler(&fakeFetcher{})
	rec := httptest.NewRecorder()
	h.GetTrends(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trends?signal=bogus_signal", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid signal)", rec.Code)
	}
}

// TestTrends_InvalidRange 는 파싱 불가하거나 비양수인 range/step 에 400 을 돌려주는지 검증한다.
func TestTrends_InvalidRange(t *testing.T) {
	h := NewTrendsHandler(&fakeFetcher{series: trendSeries()})
	for _, path := range []string{
		"/api/v1/trends?signal=noisy_neighbor_intensity&range=garbage",
		"/api/v1/trends?signal=noisy_neighbor_intensity&step=-5m",
	} {
		rec := httptest.NewRecorder()
		h.GetTrends(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d want 400", path, rec.Code)
		}
	}
}

// TestTrends_QueryError 는 range 쿼리 실패 시 500 을 돌려주는지 검증한다.
func TestTrends_QueryError(t *testing.T) {
	h := NewTrendsHandler(&fakeFetcher{err: errors.New("prometheus unreachable")})
	rec := httptest.NewRecorder()
	h.GetTrends(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trends?signal=noisy_neighbor_intensity", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (fetch 실패)", rec.Code)
	}
}

// TestTrends_NilFetcher 는 fetcher 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestTrends_NilFetcher(t *testing.T) {
	h := NewTrendsHandler(nil)
	rec := httptest.NewRecorder()
	h.GetTrends(rec, httptest.NewRequest(http.MethodGet, "/api/v1/trends?signal=noisy_neighbor_intensity", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp TrendsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Series) != 0 {
		t.Errorf("series=%d want 0 (nil fetcher)", len(resp.Series))
	}
}
