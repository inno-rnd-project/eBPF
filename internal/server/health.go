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

// NewHandler는 모든 옵저버빌리티 에이전트가 공유하는 HTTP 핸들러를 구성한다.
// serviceName은 `/` JSON 응답의 `name` 필드로 사용되며 에이전트 정체성을 드러낸다.
func NewHandler(serviceName string, reg *prometheus.Registry, ready ReadyFunc) http.Handler {
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

	// `/`는 ServeMux에서 catch-all 역할을 하므로, 루트 응답을 의도한 경로에만
	// 내려주기 위해 정확 매칭 가드를 둔다. 그 외 경로는 404로 처리한다.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"name":    serviceName,
			"metrics": "/metrics",
			"health":  "/healthz",
			"ready":   "/readyz",
		}); err != nil {
			log.Printf("root handler encode error: %v", err)
		}
	})

	return mux
}
