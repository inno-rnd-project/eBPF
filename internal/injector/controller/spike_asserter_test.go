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

// TestPromSpikeAsserter_HitOnce 는 단일 query 에서 firing alert 가 반환 되는 동작 을 검증 한다.
func TestPromSpikeAsserter_HitOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[
			{"metric":{"alertname":"CPUThrottleSpikeDetected"}},
			{"metric":{"alertname":"GPUUtilSpikeDetected"}}
		]}}`))
	}))
	defer srv.Close()

	a := NewPromSpikeAsserter(srv.URL)
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

// TestPromSpikeAsserter_NoHit 는 firing alert 가 없을 때 빈 slice 와 nil error 가 반환 되는 동작 을
// 검증 한다.
func TestPromSpikeAsserter_NoHit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
	}))
	defer srv.Close()

	a := NewPromSpikeAsserter(srv.URL)
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

	a := NewPromSpikeAsserter(srv.URL)
	got, err := a.Observe(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "CPUThrottleSpikeDetected" {
		t.Errorf("alerts=%v want one CPUThrottleSpikeDetected", got)
	}
}
