package apicommon

import (
	"log"
	"net/http"
	"time"
)

// LoggingMiddleware는 모든 API 요청의 method와 path와 status와 duration을 단일 라인으로 로그한다.
// 본 미들웨어는 4 agent 의 /api/v1/* 라우터 의 outermost 위치에 적용해 panic recovery 와 함께
// 모든 응답이 trace 되도록 한다.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)
		log.Printf("apicommon: %s %s -> %d (%s)", r.Method, r.URL.Path, ww.status, time.Since(start))
	})
}

// RecoverMiddleware 는 핸들러의 panic 을 잡아 500 응답으로 변환한다. 본 미들웨어가 없으면 단일
// 핸들러 panic 이 agent 전체 HTTP server 를 다운시킬 위험 이 있다.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("apicommon: panic recovered in %s %s: %v", r.Method, r.URL.Path, rec)
				WriteError(w, http.StatusInternalServerError, "internal_panic", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// CORSMiddleware 는 자체 dashboard 가 다른 origin 에서 API 호출 시 CORS preflight 와 헤더 추가를
// 처리한다. cluster 내부 통신 가정 이라 모든 origin 허용 (*) 기본 채택. 외부 노출 시 별도 설정
// 필요하면 본 미들웨어 를 wrapping 하는 방식으로 strict 정책 추가 가능.
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Chain 은 outermost 부터 innermost 순으로 미들웨어를 적용한다. 표준 순서: Logging -> Recover ->
// CORS -> Handler. 호출 예: apicommon.Chain(handler, apicommon.LoggingMiddleware,
// apicommon.RecoverMiddleware, apicommon.CORSMiddleware).
func Chain(handler http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}

// statusRecorder 는 LoggingMiddleware 의 status 캡처 용 ResponseWriter wrapper 다.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}
