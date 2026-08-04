package apicommon

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGzipMiddleware_CompressesWhenAccepted 는 gzip 을 수락한 소비자에게 압축 응답이 가고 해제 시
// 원문이 복원되는지 검증한다 (#411).
func TestGzipMiddleware_CompressesWhenAccepted(t *testing.T) {
	payload := strings.Repeat(`{"pod":"p","namespace":"ns"},`, 200)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payload))
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	GzipMiddleware(h).ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding=%q want gzip", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Errorf("Vary=%q want Accept-Encoding", rec.Header().Get("Vary"))
	}
	if rec.Body.Len() >= len(payload) {
		t.Errorf("압축 크기=%d 원문=%d (압축 효과 없음)", rec.Body.Len(), len(payload))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != payload {
		t.Error("해제 결과가 원문과 다름")
	}
}

// TestGzipMiddleware_PlainWhenNotAccepted 는 gzip 미수락 소비자에게 종전과 동일한 평문이 가는지
// 검증한다 (호환 유지).
func TestGzipMiddleware_PlainWhenNotAccepted(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain"))
	})
	rec := httptest.NewRecorder()
	GzipMiddleware(h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil))
	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding=%q want 미부착", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.String() != "plain" {
		t.Errorf("body=%q want plain", rec.Body.String())
	}
}
