package correlation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPrometheusInstantQuerier_Query 는 vector 응답을 InstantSample 슬라이스로 파싱하는지 검증한다.
func TestPrometheusInstantQuerier_Query(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"node":"worker2"},"value":[1782700000,"0.78"]},
			{"metric":{"node":"gpu-1"},"value":[1782700000,"0.30"]}
		]}}`))
	}))
	defer srv.Close()

	q, err := NewPrometheusInstantQuerier(srv.URL, time.Second)
	if err != nil {
		t.Fatalf("new querier: %v", err)
	}
	got, err := q.Query(context.Background(), "node:cpu_pressure_score:5m")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Labels["node"] != "worker2" || got[0].Value != 0.78 {
		t.Errorf("sample[0]=%+v want worker2/0.78", got[0])
	}
}

// TestPrometheusInstantQuerier_EmptyVector 는 결과가 빈 vector 일 때 에러 없이 빈 슬라이스를 돌려주어
// synthesis API 가 데이터 부재를 graceful 하게 처리하게 하는지 검증한다.
func TestPrometheusInstantQuerier_EmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	q, _ := NewPrometheusInstantQuerier(srv.URL, time.Second)
	got, err := q.Query(context.Background(), "absent_metric")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len=%d want 0 (빈 vector)", len(got))
	}
}

// TestPrometheusInstantQuerier_NonVector 는 matrix 등 vector 가 아닌 resultType 을 에러로 처리하는지
// 검증한다.
func TestPrometheusInstantQuerier_NonVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	q, _ := NewPrometheusInstantQuerier(srv.URL, time.Second)
	if _, err := q.Query(context.Background(), "x"); err == nil {
		t.Errorf("err=nil want non-nil (matrix resultType 거부)")
	}
}

// TestPrometheusInstantQuerier_PrometheusError 는 status!=success 응답을 에러로 처리하는지 검증한다.
func TestPrometheusInstantQuerier_PrometheusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	}))
	defer srv.Close()

	q, _ := NewPrometheusInstantQuerier(srv.URL, time.Second)
	if _, err := q.Query(context.Background(), "bad{"); err == nil {
		t.Errorf("err=nil want non-nil (400 / error status)")
	}
}

// TestPrometheusInstantQuerier_QueryTime 는 #235 의 시점 지정 조회가 Prometheus instant query 의
// time 파라미터로 전달되고, 미지정 시 파라미터가 붙지 않는지 검증한다.
func TestPrometheusInstantQuerier_QueryTime(t *testing.T) {
	var gotTime string
	var hasTime bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTime = r.URL.Query().Get("time")
		_, hasTime = r.URL.Query()["time"]
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	q, err := NewPrometheusInstantQuerier(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1783400000, 0)
	if _, err := q.Query(WithQueryTime(context.Background(), at), "up"); err != nil {
		t.Fatal(err)
	}
	if gotTime != "1783400000" {
		t.Errorf("time=%q want 1783400000", gotTime)
	}
	if _, err := q.Query(context.Background(), "up"); err != nil {
		t.Fatal(err)
	}
	if hasTime {
		t.Errorf("미지정인데 time 파라미터가 전송됨")
	}
}

// TestPrometheusInstantQuerier_ResponseSizeLimit 은 instant 응답이 전용 상한 (maxInstantResponseBytes,
// range 용 100MiB 와 분리) 을 넘으면 에러로 격상되는지 검증한다 (#406).
func TestPrometheusInstantQuerier_ResponseSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, maxInstantResponseBytes+1024))
	}))
	defer srv.Close()
	q, err := NewPrometheusInstantQuerier(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Query(context.Background(), "up"); err == nil {
		t.Error("err=nil want size limit error")
	}
}
