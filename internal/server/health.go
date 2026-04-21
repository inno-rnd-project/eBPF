package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ReadyFunc는 agent가 현재 readiness 조건을 만족하는지 반환한다.
// 두 번째 반환값은 미충족 시 원인을 설명하는 문자열이다.
type ReadyFunc func() (bool, string)

func NewHandler(reg *prometheus.Registry, ready ReadyFunc) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if ok, reason := ready(); !ok {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "not ready: %s\n", reason)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"name":    "netobs-agent",
			"metrics": "/metrics",
			"health":  "/healthz",
			"ready":   "/readyz",
		}); err != nil {
			log.Printf("root handler encode error: %v", err)
		}
	})

	return mux
}
