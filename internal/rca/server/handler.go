// Package server 는 rca-summarizer 의 HTTP mux 와 stub handler 를 정의한다. mapping registry,
// Top-N source, in-memory Store 등 비즈니스 로직은 후속 commit 에서 본 mux 의 /webhook 과
// /rca handler 에 wire-up 된다. correlation-exporter 의 custom mux 패턴을 차용해 readiness
// gate 와 prometheus registry 노출을 동일 구조로 둔다.
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Options 는 NewMux 가 받는 외부 의존을 묶는다. Webhook 과 RCA 는 후속 commit 에서 mapping +
// Store + Sources 로 채워질 핸들러다. 본 commit 의 skeleton 은 두 핸들러를 nil 로 받으면 501
// Not Implemented 를 돌려줘 lifecycle 만 검증 가능한 상태로 둔다.
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

	mux.Handle("/webhook", methodGate(http.MethodPost, opts.Webhook))
	mux.Handle("/rca", methodGate(http.MethodGet, opts.RCA))

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
// 405 Method Not Allowed 를 반환한다. skeleton commit 에서 inner=nil 케이스가 lifecycle 검증을
// 통과하면서도 운영자가 본 endpoint 가 미구현임을 즉시 알 수 있게 한다.
func methodGate(method string, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if inner == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		inner.ServeHTTP(w, r)
	})
}
