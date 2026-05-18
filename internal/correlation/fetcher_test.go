package correlation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// matrixResponse 는 query_range 정상 응답을 모방하는 helper 다. (timestamp_seconds, value_string)
// 쌍이 들어온 순서대로 한 series 의 values 로 들어간다. json.Marshal 로 직렬화해 float 포맷 정확성
// 을 표준 라이브러리에 위임한다.
func matrixResponse(seriesLabels []map[string]string, valuesBySeries [][][2]any) string {
	type item struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	}
	result := make([]item, len(seriesLabels))
	for i, labels := range seriesLabels {
		result[i] = item{Metric: labels, Values: valuesBySeries[i]}
	}
	envelope := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "matrix",
			"result":     result,
		},
	}
	b, _ := json.Marshal(envelope)
	return string(b)
}

// TestPrometheusFetcher_ParsesMatrixResponse 는 정상 query_range 응답을 LabeledSeries 로 정확히
// 파싱하는지 검증한다.
func TestPrometheusFetcher_ParsesMatrixResponse(t *testing.T) {
	resp := matrixResponse(
		[]map[string]string{
			{"node": "n1", "src_pod": "p1"},
			{"node": "n2", "src_pod": "p2"},
		},
		[][][2]any{
			{{1700000000, "1.5"}, {1700000030, "2.5"}},
			{{1700000000, "3.0"}, {1700000030, "4.0"}},
		},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != "up" {
			t.Errorf("query=%q want up", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	f := NewPrometheusFetcher(srv.URL, 2*time.Second)
	got, err := f.Fetch(context.Background(), "up",
		time.Unix(1700000000, 0), time.Unix(1700000030, 0), 30*time.Second)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("series count=%d want 2", len(got))
	}
	if got[0].Metric != "up" || got[1].Metric != "up" {
		t.Errorf("metric must be query string for all series")
	}
	if len(got[0].Series.Samples) != 2 {
		t.Errorf("samples count=%d want 2", len(got[0].Series.Samples))
	}
	if got[0].Series.Samples[0].Value != 1.5 || got[0].Series.Samples[1].Value != 2.5 {
		t.Errorf("samples=%v want [1.5, 2.5]", got[0].Series.Samples)
	}
	if got[0].Series.Samples[0].TimestampMs != 1700000000*1000 {
		t.Errorf("timestamp=%d want 1700000000000", got[0].Series.Samples[0].TimestampMs)
	}
}

// TestPrometheusFetcher_EmptyResult 는 query 가 매칭되는 시계열이 없을 때 빈 슬라이스를 반환하고
// 에러는 내지 않는지 검증한다.
func TestPrometheusFetcher_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	f := NewPrometheusFetcher(srv.URL, 2*time.Second)
	got, err := f.Fetch(context.Background(), "up",
		time.Unix(0, 0), time.Unix(60, 0), 30*time.Second)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("series count=%d want 0", len(got))
	}
}

// TestPrometheusFetcher_HTTPError 는 5xx 응답에서 에러로 변환되는지 검증한다.
func TestPrometheusFetcher_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := NewPrometheusFetcher(srv.URL, 2*time.Second)
	_, err := f.Fetch(context.Background(), "up",
		time.Unix(0, 0), time.Unix(60, 0), 30*time.Second)
	if err == nil {
		t.Errorf("err=nil want non-nil for 500 response")
	}
}

// TestPrometheusFetcher_BadJSON 은 응답이 jSON 디코딩 실패할 때 에러를 반환하는지 검증한다.
func TestPrometheusFetcher_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json at all`))
	}))
	defer srv.Close()

	f := NewPrometheusFetcher(srv.URL, 2*time.Second)
	_, err := f.Fetch(context.Background(), "up",
		time.Unix(0, 0), time.Unix(60, 0), 30*time.Second)
	if err == nil {
		t.Errorf("err=nil want non-nil for malformed JSON")
	}
}

// TestPrometheusFetcher_PrometheusError 는 status=error 응답이 에러로 변환되는지 검증한다.
func TestPrometheusFetcher_PrometheusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error at char 5"}`))
	}))
	defer srv.Close()

	f := NewPrometheusFetcher(srv.URL, 2*time.Second)
	_, err := f.Fetch(context.Background(), "syntax !! error",
		time.Unix(0, 0), time.Unix(60, 0), 30*time.Second)
	if err == nil {
		t.Errorf("err=nil want non-nil for prometheus status=error")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("err=%v want to contain prometheus message", err)
	}
}
