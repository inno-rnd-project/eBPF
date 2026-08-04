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

// corsAllowedOrigins 는 CORS 응답을 허용할 origin allow-list 다 (#409). main 이 startup 시점에
// SetCORSAllowedOrigins 로 1회 설정하고 이후 읽기 전용이다. 비어 있으면 (기본) CORS 헤더를 아예
// 부착하지 않아 브라우저의 cross-origin 읽기가 전부 차단된다. 종전의 무조건 * 부착은 운영자가
// kubectl port-forward 를 열어 둔 동안 방문한 임의 웹페이지가 브라우저 경유로 클러스터 인벤토리를
// 읽어갈 수 있는 구멍이었다.
var corsAllowedOrigins = map[string]struct{}{}

// SetCORSAllowedOrigins 는 CORS origin allow-list 를 설정한다. main 이 env/flag 값으로 startup
// 시점에 1회 호출한다 (serve 시작 전이라 동기화 불필요). 빈 슬라이스는 기본 (미부착) 유지다.
func SetCORSAllowedOrigins(origins []string) {
	m := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		if o != "" {
			m[o] = struct{}{}
		}
	}
	corsAllowedOrigins = m
}

// CORSMiddleware 는 allow-list 에 등록된 origin 의 요청에만 CORS 헤더를 부착한다 (#409). 요청
// Origin 이 allow-list 와 정확히 일치할 때만 그 origin 을 echo 하고 (와일드카드 미사용), Vary:
// Origin 으로 캐시 오염을 막는다. allow-list 미설정 (기본) 이나 미등록 origin 은 헤더가 없어
// 브라우저가 응답 읽기를 차단한다. OPTIONS preflight 는 allow-list 매칭 여부와 무관하게 204 로
// 종결한다 (매칭 실패면 CORS 헤더가 없어 브라우저가 본요청을 보내지 않는다).
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if _, ok := corsAllowedOrigins[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.Header().Add("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MethodGuard 는 GET 과 OPTIONS 외 메서드에 405 와 표준 ErrorBody 를 반환한다 (#409). 본 API 는
// 전부 조회 전용인데 종전에는 DELETE 나 POST 도 전 쿼리를 실행해 감사 로그에 쓰기 시도 성공처럼
// 남았다. Allow 헤더로 허용 메서드를 명시한다 (RFC 9110).
func MethodGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodOptions {
			w.Header().Set("Allow", "GET, OPTIONS")
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET 과 OPTIONS 만 지원합니다")
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
