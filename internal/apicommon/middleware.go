package apicommon

import (
	"log"
	"net/http"
	"sync"
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
		// #409 경로는 percent-decode 된 사용자 입력이라 %q 로 이스케이프해 개행 주입으로 위조 로그
		// 라인을 만드는 것을 차단한다.
		log.Printf("apicommon: %s %q -> %d (%s)", r.Method, r.URL.Path, ww.status, time.Since(start))
	})
}

// RecoverMiddleware 는 핸들러의 panic 을 잡아 500 응답으로 변환한다. 본 미들웨어가 없으면 단일
// 핸들러 panic 이 agent 전체 HTTP server 를 다운시킬 위험 이 있다.
func RecoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// #409 경로 이스케이프는 LoggingMiddleware 와 동일 사유다.
				log.Printf("apicommon: panic recovered in %s %q: %v", r.Method, r.URL.Path, rec)
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
// allow-list 에 "*" 가 있으면 전 origin 개방 상태를 기동 로그로 경고해, 진단용 임시 설정이 운영에
// 남아 있는 것을 운영자가 즉시 인지하게 한다.
func SetCORSAllowedOrigins(origins []string) {
	m := make(map[string]struct{}, len(origins))
	for _, o := range origins {
		if o != "" {
			m[o] = struct{}{}
		}
	}
	corsAllowedOrigins = m
	if _, wildcard := m["*"]; wildcard {
		log.Printf("apicommon: warn: cors: 전 origin 개방 (\"*\") 상태다. 임의 사이트가 브라우저 경유로 API 를 읽을 수 있으니 진단 목적 한정으로만 유지하고 명시 목록으로 되돌린다")
	}
}

// corsDenied 는 거부된 origin 의 첫 관측 로그용 집합이다 (#409 후속). CORS 거부는 서버가 200 을
// 돌려주고 브라우저만 읽기를 차단하므로 서버 측 흔적이 없으면 소비자 장애의 진단이 브라우저
// 콘솔에서만 가능하다. origin 당 1회만 로그하고 집합 크기에 상한을 둬 임의 origin 스팸으로 인한
// 메모리 증가와 로그 폭주를 막는다.
var (
	corsDeniedMu  sync.Mutex
	corsDenied    = map[string]struct{}{}
	corsDeniedCap = 64
)

// logDeniedOriginOnce 는 미등록 origin 을 첫 관측 시 1회 로그한다. 운영자는 이 로그로 어떤 origin
// 을 API_CORS_ALLOW_ORIGINS 에 등재해야 하는지 서버 측에서 즉시 판별한다.
func logDeniedOriginOnce(origin string) {
	corsDeniedMu.Lock()
	defer corsDeniedMu.Unlock()
	if _, seen := corsDenied[origin]; seen {
		return
	}
	if len(corsDenied) >= corsDeniedCap {
		return
	}
	corsDenied[origin] = struct{}{}
	log.Printf("apicommon: cors: denied origin %q (API_CORS_ALLOW_ORIGINS 에 미등재)", origin)
}

// CORSMiddleware 는 allow-list 에 등록된 origin 의 요청에만 CORS 헤더를 부착한다 (#409). 요청
// Origin 이 allow-list 와 정확히 일치할 때만 그 origin 을 echo 하고 (와일드카드 미사용), Vary:
// Origin 으로 캐시 오염을 막는다. allow-list 미설정 (기본) 이나 미등록 origin 은 헤더가 없어
// 브라우저가 응답 읽기를 차단하며, 거부는 origin 당 1회 서버 로그로 남는다. OPTIONS preflight 는
// allow-list 매칭 여부와 무관하게 204 로 종결한다 (매칭 실패면 CORS 헤더가 없어 브라우저가
// 본요청을 보내지 않는다).
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			// #409 후속 와일드카드 opt-in. allow-list 에 "*" 가 있으면 요청 origin 을 그대로 echo 한다.
			//
			// 위험 경고: origin echo 는 literal "*" 가 credentials 와 함께 쓰일 수 없다는 브라우저
			// 제약을 우회하는 방식이라 literal "*" 보다 위험한 성질이다. 현재는 본 패키지가
			// Access-Control-Allow-Credentials 를 어디에서도 부착하지 않아 브라우저가 인증 정보를
			// 실은 cross-origin 응답 읽기를 차단하므로 실해가 없다. 향후 인증을 도입해 그 헤더를
			// 추가할 때는 allow-list 에 "*" 가 있으면 임의 사이트가 인증된 응답을 읽게 되므로, 반드시
			// 본 경로를 차단하고 (wildcard 와 credentials 동시 허용 금지) 명시 목록만 남겨야 한다.
			// 본 opt-in 은 소비자 origin 확정 전의 임시 개방과 원인 판별용이며 운영 기본값은 명시
			// 목록이다. 설정 시 기동 로그로 경고가 남는다 (SetCORSAllowedOrigins).
			_, allowed := corsAllowedOrigins[origin]
			if !allowed {
				_, allowed = corsAllowedOrigins["*"]
			}
			if allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
				// preflight 가 요청한 헤더를 그대로 허용한다. 종전 Content-Type 고정은 소비자가 임의
				// 헤더 (Cache-Control, X-* 등) 를 실으면 preflight 가 실패해 요청이 차단됐다.
				if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
					w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
				} else {
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				}
				w.Header().Set("Access-Control-Max-Age", "600")
				w.Header().Add("Vary", "Origin")
			} else {
				logDeniedOriginOnce(origin)
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// MethodGuard 는 안전한 조회 메서드 (GET, HEAD, OPTIONS) 외 메서드에 405 와 표준 ErrorBody 를
// 반환한다 (#409). 본 API 는 전부 조회 전용인데 종전에는 DELETE 나 POST 도 전 쿼리를 실행해 감사
// 로그에 쓰기 시도 성공처럼 남았다. HEAD 는 본문 없는 GET 이라 Go 의 http 서버가 GET 핸들러를
// 실행하고 본문만 버리는 방식으로 자동 처리하며, 헬스체크와 프록시와 일부 HTTP 클라이언트가
// 도달성 확인에 쓰는 표준 안전 메서드라 차단하면 소비자가 끊긴다 (#409 직후 405 회귀 확인).
// Allow 헤더로 허용 메서드를 명시한다 (RFC 9110).
func MethodGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET 과 HEAD 와 OPTIONS 만 지원합니다")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Chain 은 outermost 부터 innermost 순으로 미들웨어를 적용한다. 표준 순서: Logging -> Recover ->
// MethodGuard -> CORS -> Handler (#409). MethodGuard 가 GET 과 OPTIONS 를 통과시키므로 preflight 가
// CORS 에 도달해 204 로 종결된다 (순서를 뒤집으면 preflight 가 405 로 막혀 CORS 가 무력화된다).
// 호출 예: apicommon.Chain(handler, apicommon.LoggingMiddleware, apicommon.RecoverMiddleware,
// apicommon.MethodGuard, apicommon.CORSMiddleware).
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
