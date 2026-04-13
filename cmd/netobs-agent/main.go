package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"netobs/internal/config"
	ebpfx "netobs/internal/ebpf"
	"netobs/internal/metrics"
	"netobs/internal/server"
	"netobs/internal/types"

	"github.com/prometheus/client_golang/prometheus"
)

func main() {
	cfg := config.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	metrics.Register(reg)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server.NewHandler(reg),
	}

	go func() {
		log.Printf("serving metrics on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server error: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	events := make(chan types.Event, 4096)
	errCh := make(chan error, 1)

	go func() {
		errCh <- ebpfx.Run(ctx, cfg.TargetIP, events)
		close(errCh)
	}()

	for events != nil || errCh != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}

			metrics.Record(ev)

			if cfg.PrintEvents {
				log.Printf(
					"stage=%s comm=%s pid=%d tid=%d %s:%d -> %s:%d latency_us=%d ret=%d reason=%d cgroup=%d",
					types.StageName(ev.Stage),
					types.CommString(ev.Comm),
					ev.Pid,
					ev.Tid,
					types.U32ToIPv4(ev.Saddr),
					ev.Sport,
					types.U32ToIPv4(ev.Daddr),
					ev.Dport,
					ev.LatencyUs,
					ev.Ret,
					ev.Reason,
					ev.CgroupID,
				)
			}

		case err, ok := <-errCh:
			if ok && err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("ebpf runner error: %v", err)
			}
			errCh = nil

		case <-ctx.Done():
		}
	}

	log.Printf("exiting")
}
