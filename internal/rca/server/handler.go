// Package server 는 rca-summarizer 의 HTTP mux 와 endpoint handler 를 정의한다. Options 에
// 주입된 webhook / rca handler 가 mapping registry, Top-N source, in-memory Store 와 결합되어
// /webhook, /rca 두 경로에서 실제 RCA 산정 흐름을 처리한다. nil handler 가 주입된 경로는 501
// Not Implemented 를 돌려줘 부분 구성 단계의 lifecycle 검증을 허용한다. correlation-exporter
// 의 custom mux 패턴을 차용해 readiness gate 와 prometheus registry 노출을 동일 구조로 둔다.
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"netobs/internal/apicommon"
)

// Options 는 NewMux 가 받는 외부 의존을 묶는다. Webhook 과 RCA 는 mapping registry + Store +
// Sources 로 wire-up 된 실 핸들러를 받는다. 두 핸들러가 nil 로 비어 있으면 501 Not Implemented
// 를 돌려줘 부분 구성 또는 단위 테스트의 lifecycle 만 검증 가능한 상태로 둔다.
type Options struct {
	Registry *prometheus.Registry
	Ready    *atomic.Bool
	Webhook  http.Handler // POST /webhook payload 핸들러. nil 이면 501.
	RCA      http.Handler // GET /rca?alert=<name> 응답 핸들러. nil 이면 501.
}

// NewMux 는 rca-summarizer 의 모든 endpoint 를 등록한 mux 를 반환한다. /metrics, /healthz,
// /readyz, /webhook, /rca 5 endpoint 와 / catch-all 의 정체성 JSON 응답을 노출한다.
func NewMux(opts Options) http.Handler {
	mux := http.NewServeMux()

	if opts.Registry != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{Registry: opts.Registry}))
	}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if opts.Ready != nil && !opts.Ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})

	// #447 correlation API 와 규약 통일. API 성 경로(/webhook, /rca)에 요청 로그와 panic 복구
	// 미들웨어를 적용하고 에러 응답을 표준 ErrorBody 로 통일한다. /healthz 와 /readyz 는 kubelet
	// probe 전용 평문 규약이라 제외한다. MethodGuard 는 조회 전용(GET/HEAD/OPTIONS)이라 POST 를
	// 받는 /webhook 에 쓸 수 없어 로컬 methodGate 가 method 게이트를 유지한다.
	mux.Handle("/webhook", apicommon.Chain(
		methodGate(http.MethodPost, opts.Webhook),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
	))
	mux.Handle("/rca", apicommon.Chain(
		methodGate(http.MethodGet, opts.RCA),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
	))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"name":    "rca-summarizer",
			"metrics": "/metrics",
			"health":  "/healthz",
			"ready":   "/readyz",
			"webhook": "/webhook",
			"rca":     "/rca?alert=<name>",
		}); err != nil {
			log.Printf("root handler encode: %v", err)
		}
	})

	return mux
}

// methodGate 는 inner 가 nil 이면 501 Not Implemented 를 반환하고, HTTP method 가 기대와 다르면
// 405 Method Not Allowed 를 반환한다. nil inner 케이스는 부분 구성 단계의 lifecycle 검증과
// 단위 테스트가 endpoint 가 의도적으로 비어 있음을 명확히 구분 가능하게 한다.
func methodGate(method string, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			apicommon.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "허용 method: "+method)
			return
		}
		if inner == nil {
			apicommon.WriteError(w, http.StatusNotImplemented, "not_implemented", "핸들러가 구성되지 않았습니다")
			return
		}
		inner.ServeHTTP(w, r)
	})
}
