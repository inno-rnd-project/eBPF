package apicommon

import (
	"bytes"
	"log"
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
		if got := rec.Header().Get("Allow"); got != "GET, HEAD, OPTIONS" {
			t.Errorf("%s Allow=%q want GET, HEAD, OPTIONS", m, got)
		}
		if !strings.Contains(rec.Body.String(), "method_not_allowed") {
			t.Errorf("%s body=%q want method_not_allowed ErrorBody", m, rec.Body.String())
		}
	}
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(m, "/api/v1/x", nil)
		rec := httptest.NewRecorder()
		MethodGuard(okHandler()).ServeHTTP(rec, req)
		if rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s 가 405 로 차단됨", m)
		}
	}
}

// TestLoggingMiddleware_PathEscaped 는 percent-decode 된 경로의 개행이 이스케이프되어 위조 로그
// 라인 주입이 불가한지 검증한다 (#409).
func TestLoggingMiddleware_PathEscaped(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	req.URL.Path = "/api/v1/x\ninjected: FAKE LINE"
	rec := httptest.NewRecorder()
	LoggingMiddleware(okHandler()).ServeHTTP(rec, req)

	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Errorf("로그 라인 수=%d want 1 (개행 주입 차단): %q", strings.Count(out, "\n"), out)
	}
	if !strings.Contains(out, `\n`) {
		t.Errorf("이스케이프된 개행 미포함: %q", out)
	}
}

// TestCORSMiddleware_DeniedOriginLoggedOnce 는 미등록 origin 거부가 origin 당 1회만 서버 로그로
// 남는지 검증한다 (#409 후속). CORS 거부는 브라우저만 읽기를 차단해 서버 흔적이 없으면 소비자
// 장애 진단이 브라우저 콘솔에서만 가능했다.
func TestCORSMiddleware_DeniedOriginLoggedOnce(t *testing.T) {
	SetCORSAllowedOrigins(nil)
	corsDeniedMu.Lock()
	corsDenied = map[string]struct{}{}
	corsDeniedMu.Unlock()

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
		req.Header.Set("Origin", "http://unregistered.example:3000")
		CORSMiddleware(okHandler()).ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := strings.Count(buf.String(), "denied origin"); got != 1 {
		t.Errorf("denied 로그 %d회 want 1 (origin 당 1회)", got)
	}
	if !strings.Contains(buf.String(), "unregistered.example:3000") {
		t.Errorf("로그에 origin 미포함: %q", buf.String())
	}
}

// TestCORSMiddleware_WildcardOptIn 은 allow-list 에 "*" 가 있으면 임의 origin 을 echo 하는지 검증
// 한다 (#409 후속, 임시 개방과 원인 판별 실험용). literal * 가 아니라 echo 라 credentials 요청에도
// 동작한다.
func TestCORSMiddleware_WildcardOptIn(t *testing.T) {
	SetCORSAllowedOrigins([]string{"*"})
	defer SetCORSAllowedOrigins(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	req.Header.Set("Origin", "http://any.example:1234")
	rec := httptest.NewRecorder()
	CORSMiddleware(okHandler()).ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://any.example:1234" {
		t.Errorf("ACAO=%q want 요청 origin echo", got)
	}
}

// TestCORSMiddleware_PreflightHeadersEchoed 는 preflight 가 요청한 헤더가 그대로 허용되는지 검증
// 한다. 종전 Content-Type 고정은 소비자가 임의 헤더를 실으면 preflight 실패로 요청이 차단됐다.
func TestCORSMiddleware_PreflightHeadersEchoed(t *testing.T) {
	SetCORSAllowedOrigins([]string{"http://dash.example"})
	defer SetCORSAllowedOrigins(nil)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/x", nil)
	req.Header.Set("Origin", "http://dash.example")
	req.Header.Set("Access-Control-Request-Headers", "cache-control, x-trace-id")
	rec := httptest.NewRecorder()
	CORSMiddleware(okHandler()).ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "cache-control, x-trace-id" {
		t.Errorf("Allow-Headers=%q want 요청 헤더 echo", got)
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status=%d want 204", rec.Code)
	}
}
