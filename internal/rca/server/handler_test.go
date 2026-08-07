package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestNewMux_HealthAndReadiness 는 /healthz 가 항상 200 을 돌려주고 /readyz 는 atomic.Bool 가드
// 결과를 따르는지 검증한다. correlation-exporter 와 동일 패턴의 회귀 가드다.
func TestNewMux_HealthAndReadiness(t *testing.T) {
	var ready atomic.Bool
	mux := NewMux(Options{Registry: prometheus.NewRegistry(), Ready: &ready})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status=%d; want 200", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz status=%d before ready; want 503", resp.StatusCode)
	}

	ready.Store(true)
	resp, err = http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("readyz after ready: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("readyz status=%d after ready; want 200", resp.StatusCode)
	}
}

// TestNewMux_WebhookMethodGate 는 /webhook 이 GET 등 비-POST 요청에 405 를 돌려주고 inner=nil
// 일 때 POST 에는 501 을 돌려주는지 검증한다.
func TestNewMux_WebhookMethodGate(t *testing.T) {
	mux := NewMux(Options{Registry: prometheus.NewRegistry()})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/webhook")
	if err != nil {
		t.Fatalf("GET /webhook: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /webhook status=%d; want 405", resp.StatusCode)
	}

	resp, err = http.Post(srv.URL+"/webhook", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /webhook: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST /webhook stub status=%d; want 501", resp.StatusCode)
	}
}

// TestNewMux_RCAMethodGate 는 /rca 가 POST 에 405 를 돌려주고 inner=nil 일 때 GET 에는 501 을
// 돌려주는지 검증한다.
func TestNewMux_RCAMethodGate(t *testing.T) {
	mux := NewMux(Options{Registry: prometheus.NewRegistry()})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/rca", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /rca: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /rca status=%d; want 405", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/rca?alert=Foo")
	if err != nil {
		t.Fatalf("GET /rca: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("GET /rca stub status=%d; want 501", resp.StatusCode)
	}
}

// TestNewMux_WebhookInvokesInner 는 Options.Webhook 에 주입된 핸들러가 POST 흐름에서 실제로
// 호출되는지 검증한다. 후속 commit 의 mapping registry 통합 흐름의 회귀 가드다.
func TestNewMux_WebhookInvokesInner(t *testing.T) {
	var called bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	})
	mux := NewMux(Options{Registry: prometheus.NewRegistry(), Webhook: inner})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /webhook: %v", err)
	}
	_ = resp.Body.Close()
	if !called {
		t.Errorf("inner webhook not invoked")
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status=%d; want 202", resp.StatusCode)
	}
}
