package apicommon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestCORSMiddleware_DefaultNoHeaders 는 allow-list 미설정 (기본) 시 어떤 origin 에도 CORS 헤더가
// 부착되지 않는지 검증한다 (#409). 종전 무조건 * 부착은 port-forward 열린 동안 임의 웹페이지의
// 브라우저 경유 읽기를 허용했다.
func TestCORSMiddleware_DefaultNoHeaders(t *testing.T) {
	SetCORSAllowedOrigins(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	CORSMiddleware(okHandler()).ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO=%q want 미부착", got)
	}
}

// TestCORSMiddleware_AllowListEcho 는 등록 origin 만 echo 되고 (와일드카드 미사용) Vary: Origin 이
// 붙으며, 미등록 origin 은 헤더가 없는지 검증한다.
func TestCORSMiddleware_AllowListEcho(t *testing.T) {
	SetCORSAllowedOrigins([]string{"https://dash.example"})
	defer SetCORSAllowedOrigins(nil)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	req.Header.Set("Origin", "https://dash.example")
	rec := httptest.NewRecorder()
	CORSMiddleware(okHandler()).ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://dash.example" {
		t.Errorf("ACAO=%q want 등록 origin echo", got)
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary=%q want Origin 포함", rec.Header().Get("Vary"))
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	req2.Header.Set("Origin", "https://evil.example")
	rec2 := httptest.NewRecorder()
	CORSMiddleware(okHandler()).ServeHTTP(rec2, req2)
	if got := rec2.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("미등록 origin 에 ACAO=%q 부착", got)
	}
}

// TestCORSMiddleware_OptionsPreflight 는 OPTIONS 가 204 로 종결되는지 검증한다.
func TestCORSMiddleware_OptionsPreflight(t *testing.T) {
	SetCORSAllowedOrigins(nil)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/x", nil)
	rec := httptest.NewRecorder()
	CORSMiddleware(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status=%d want 204", rec.Code)
	}
}

// TestMethodGuard 는 GET 과 OPTIONS 외 메서드가 405 와 표준 ErrorBody, Allow 헤더를 받는지 검증한다
// (#409). 종전에는 DELETE 나 POST 도 전 쿼리를 실행했다.
func TestMethodGuard(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		req := httptest.NewRequest(m, "/api/v1/x", nil)
		rec := httptest.NewRecorder()
		MethodGuard(okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status=%d want 405", m, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != "GET, OPTIONS" {
			t.Errorf("%s Allow=%q want GET, OPTIONS", m, got)
		}
		if !strings.Contains(rec.Body.String(), "method_not_allowed") {
			t.Errorf("%s body=%q want method_not_allowed ErrorBody", m, rec.Body.String())
		}
	}
	for _, m := range []string{http.MethodGet, http.MethodOptions} {
		req := httptest.NewRequest(m, "/api/v1/x", nil)
		rec := httptest.NewRecorder()
		MethodGuard(okHandler()).ServeHTTP(rec, req)
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s 가 405 로 차단됨", m)
		}
	}
}
