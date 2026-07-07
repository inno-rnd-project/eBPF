package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRCAProxy 는 alert 파라미터 통과와 응답 status/본문 프록시를 검증한다. 허용 외 파라미터는
// 상류로 전달되지 않아야 한다.
func TestRCAProxy(t *testing.T) {
	var gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/rca" {
			t.Errorf("path=%q want /rca", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"summary":{"alert_name":"NetObsDropBurst"}}`))
	}))
	defer up.Close()

	h := NewRCAProxyHandler(up.URL, 2*time.Second)
	if h == nil {
		t.Fatal("handler nil")
	}
	rec := httptest.NewRecorder()
	h.GetRCA(rec, httptest.NewRequest(http.MethodGet, "/api/v1/rca?alert=NetObsDropBurst&evil=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if gotQuery != "alert=NetObsDropBurst" {
		t.Errorf("upstream query=%q want alert 만 통과", gotQuery)
	}
	if !strings.Contains(rec.Body.String(), "NetObsDropBurst") {
		t.Errorf("body=%q want 프록시 본문", rec.Body.String())
	}
}

// TestRCAProxy_ContentTypeForward 는 상류의 비JSON Content-Type 이 그대로 전달되는지 검증한다.
func TestRCAProxy_ContentTypeForward(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("upstream maintenance"))
	}))
	defer up.Close()

	h := NewRCAProxyHandler(up.URL, 2*time.Second)
	rec := httptest.NewRecorder()
	h.GetRCA(rec, httptest.NewRequest(http.MethodGet, "/api/v1/rca", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type=%q want 상류 값 전달", ct)
	}
}

// TestRCAProxy_UpstreamDown 은 상류 불가 시 502 를 돌려주는지 검증한다.
func TestRCAProxy_UpstreamDown(t *testing.T) {
	h := NewRCAProxyHandler("http://127.0.0.1:1", 200*time.Millisecond)
	rec := httptest.NewRecorder()
	h.GetRCA(rec, httptest.NewRequest(http.MethodGet, "/api/v1/rca", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status=%d want 502", rec.Code)
	}
}

// TestRCAProxy_InvalidURL 은 base URL 파싱 실패 시 nil 을 돌려주는지 검증한다.
func TestRCAProxy_InvalidURL(t *testing.T) {
	if h := NewRCAProxyHandler("::not-a-url", time.Second); h != nil {
		t.Errorf("handler=%v want nil", h)
	}
}
