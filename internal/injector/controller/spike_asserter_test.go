package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestPromSpikeAsserter_HitImmediately 는 첫 query 에서 firing alert 가 hit 하면 즉시 반환 하는
// 동작 을 검증 한다.
func TestPromSpikeAsserter_HitImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[
			{"metric":{"alertname":"CPUThrottleSpikeDetected"}},
			{"metric":{"alertname":"GPUUtilSpikeDetected"}}
		]}}`))
	}))
	defer srv.Close()

	a := NewPromSpikeAsserter(srv.URL, 200*time.Millisecond, 50*time.Millisecond)
	got, err := a.Observe(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sort.Strings(got)
	want := []string{"CPUThrottleSpikeDetected", "GPUUtilSpikeDetected"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("alerts=%v want=%v", got, want)
	}
}

// TestPromSpikeAsserter_TimeoutNoHit 는 polling window 만료 까지 firing alert 가 없으면 빈 slice
// 와 nil error 를 반환 하는 정상 종료 동작 을 검증 한다.
func TestPromSpikeAsserter_TimeoutNoHit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()

	a := NewPromSpikeAsserter(srv.URL, 100*time.Millisecond, 30*time.Millisecond)
	got, err := a.Observe(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("alerts=%v want empty", got)
	}
}

// TestPromSpikeAsserter_DedupAlertname 는 동일 alertname 의 multi-series 를 distinct 셋 으로
// 축약 하는 동작 을 검증 한다.
func TestPromSpikeAsserter_DedupAlertname(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[
			{"metric":{"alertname":"CPUThrottleSpikeDetected","node":"a"}},
			{"metric":{"alertname":"CPUThrottleSpikeDetected","node":"b"}}
		]}}`))
	}))
	defer srv.Close()

	a := NewPromSpikeAsserter(srv.URL, 50*time.Millisecond, 10*time.Millisecond)
	got, err := a.Observe(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "CPUThrottleSpikeDetected" {
		t.Errorf("alerts=%v want one CPUThrottleSpikeDetected", got)
	}
}
