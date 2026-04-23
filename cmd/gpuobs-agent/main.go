package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/gpuobs/collector"
	"netobs/internal/gpuobs/config"
	"netobs/internal/gpuobs/metrics"
	"netobs/internal/server"
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
		Handler: server.NewHandler("gpuobs-agent", reg, ready),
	}

	// HTTP 서버. ListenAndServe는 Shutdown 전까지 블록되는 것이 정상 동작이며,
	// 포트 바인드 실패 등 비정상 종료 시에는 fail-fast로 프로세스를 내려 메트릭 없이
	// 좀비 상태로 살아남는 상황을 막는다.
	go func() {
		log.Printf("serving gpuobs metrics on %s (node=%s)", cfg.ListenAddr, cfg.NodeName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics server error: %v", err)
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
