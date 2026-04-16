package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"netobs/internal/config"
	"netobs/internal/drop"
	ebpfx "netobs/internal/ebpf"
	"netobs/internal/metadata"
	"netobs/internal/metrics"
	"netobs/internal/server"
	"netobs/internal/types"

	"github.com/prometheus/client_golang/prometheus"
)

func podLabel(p types.PodIdentity) string {
	if p.Namespace != "" && p.PodName != "" {
		return fmt.Sprintf("%s/%s", p.Namespace, p.PodName)
	}
	if p.PodIP != "" {
		return p.PodIP
	}
	return "unknown"
}

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

	resolver := metadata.NewResolver(cfg.NodeName, cfg.MetadataRefresh)
	go resolver.Start(ctx)

	mapper := drop.NewMapper(drop.DefaultPaths(cfg.DropReasonFormatPath))

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

			enriched := resolver.Enrich(ev, mapper)
			metrics.Record(enriched)

			if cfg.PrintEvents {
				log.Printf(
					"stage=%s node=%s scope=%s dir=%s src=%s:%d(%s/%s) dst=%s:%d(%s/%s) comm=%s pid=%d tid=%d latency_us=%d ret=%d drop_reason=%s drop_category=%s cgroup=%d",
					enriched.Stage,
					enriched.ObservedNode,
					enriched.TrafficScope,
					enriched.Direction,
					enriched.SrcIPText,
					enriched.Raw.Sport,
					enriched.Src.NamespaceLabel(),
					enriched.Src.WorkloadLabel(),
					enriched.DstIPText,
					enriched.Raw.Dport,
					enriched.Dst.NamespaceLabel(),
					enriched.Dst.WorkloadLabel(),
					enriched.CommText,
					enriched.Raw.Pid,
					enriched.Raw.Tid,
					enriched.Raw.LatencyUs,
					enriched.Raw.Ret,
					enriched.DropReasonName,
					enriched.DropCategory,
					enriched.Raw.CgroupID,
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
