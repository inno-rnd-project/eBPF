package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"netobs/internal/gpuobs/collector"
	"netobs/internal/gpuobs/config"
	"netobs/internal/gpuobs/metrics"
)

func main() {
	cfg, err := config.Parse()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	metrics.Register(reg)

	var collectorReady atomic.Bool
	ready := func() (bool, string) {
		if !collectorReady.Load() {
			return false, "collector not ready"
		}
		return true, ""
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: newHandler(reg, ready),
	}

	// HTTP 서버. ListenAndServe는 Shutdown 전까지 블록되는 것이 정상 동작이다.
	go func() {
		log.Printf("serving gpuobs metrics on %s (node=%s)", cfg.ListenAddr, cfg.NodeName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server error: %v", err)
		}
	}()

	// Phase 1 collector는 placeholder로, ctx가 취소되면 즉시 종료된다.
	// Phase 2에서 NVML 폴링 루프로 대체된다.
	col := collector.New()
	errCh := make(chan error, 1)
	go func() {
		errCh <- col.Run(ctx, func() {
			collectorReady.Store(true)
		})
		close(errCh)
	}()

	// 이벤트 루프.
	// ctx가 취소되면 doneSignal을 nil로 돌려 해당 case를 비활성화한다.
	// 이후 errCh가 자연스럽게 close되면 루프가 종료되므로 busy loop 없이 drain된다.
	doneSignal := ctx.Done()
	for errCh != nil {
		select {
		case err, ok := <-errCh:
			if ok && err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("collector error: %v", err)
			}
			errCh = nil
		case <-doneSignal:
			log.Printf("shutdown signal received")
			doneSignal = nil
		}
	}

	// 이벤트 루프가 끝난 뒤 HTTP 서버를 graceful하게 종료한다.
	// main이 먼저 return되면 Shutdown이 중단될 수 있어 여기서 동기 실행한다.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("metrics server shutdown: %v", err)
	}

	log.Printf("exiting")
}

// newHandler는 gpuobs 전용 HTTP 핸들러를 구성한다.
// netobs의 internal/server 패키지는 `/` 응답에 "netobs-agent" 이름이 박혀 있어
// gpuobs가 재사용하면 정체성이 잘못 표현된다. 경로 분리를 확실히 하기 위해
// gpuobs는 자체 핸들러를 이 파일 안에 둔다.
func newHandler(reg *prometheus.Registry, ready func() (bool, string)) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ok, reason := ready(); !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "not ready: %s\n", reason)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"name":    "gpuobs-agent",
			"metrics": "/metrics",
			"health":  "/healthz",
			"ready":   "/readyz",
		}); err != nil {
			log.Printf("root handler encode error: %v", err)
		}
	})

	return mux
}
