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

	"netobs/internal/config"
	"netobs/internal/drop"
	ebpfx "netobs/internal/ebpf"
	"netobs/internal/metadata"
	"netobs/internal/metrics"
	"netobs/internal/server"
	"netobs/internal/types"

	"github.com/prometheus/client_golang/prometheus"
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
	metrics.SetPodMetricsEnabled(cfg.PodMetricsEnabled)

	var ebpfReady atomic.Bool
	resolver := metadata.NewResolver(cfg.NodeName, cfg.MetadataRefresh)

	ready := func() (bool, string) {
		if !resolver.HasSynced() {
			return false, "metadata informer not synced"
		}
		if !ebpfReady.Load() {
			return false, "ebpf not attached"
		}
		return true, ""
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server.NewHandler(reg, ready),
	}

	// HTTP server: ListenAndServe는 shutdown 전까지 블록되어야 정상 동작.
	go func() {
		log.Printf("serving metrics on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics server error: %v", err)
		}
	}()

	// Kubernetes metadata informer.
	go resolver.Start(ctx)

	mapper := drop.NewMapper(drop.DefaultPaths(cfg.DropReasonFormatPath))

	// eBPF runner. ctx가 취소되면 내부에서 ringbuf를 닫고 events 채널을 close한 뒤
	// 최종 에러를 errCh로 전달한다.
	events := make(chan types.Event, 4096)
	errCh := make(chan error, 1)

	go func() {
		errCh <- ebpfx.Run(ctx, cfg.TargetIP, events, func() {
			ebpfReady.Store(true)
		})
		close(errCh)
	}()

	// 이벤트 루프.
	// ctx가 취소되면 doneSignal을 nil로 돌려 해당 case를 비활성화한다.
	// 이후 events/errCh가 자연스럽게 close되면 루프가 종료되므로
	// busy loop 없이 정상 drain이 가능하다.
	doneSignal := ctx.Done()
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
					"stage=%s node=%s scope=%s dir=%s src=%s:%d(%s/%s uid=%s wk=%s) dst=%s:%d(%s/%s uid=%s wk=%s) comm=%s pid=%d tid=%d latency_us=%d ret=%d drop_reason=%s drop_category=%s ifindex=%d skb_iif=%d cookie=%d cgroup=%d",
					enriched.Stage,
					enriched.ObservedNode,
					enriched.TrafficScope,
					enriched.Direction,
					enriched.SrcIPText,
					enriched.Raw.Sport,
					enriched.Src.NamespaceLabel(),
					enriched.Src.WorkloadLabel(),
					enriched.Src.PodUID,
					enriched.Src.WorkloadKey(),
					enriched.DstIPText,
					enriched.Raw.Dport,
					enriched.Dst.NamespaceLabel(),
					enriched.Dst.WorkloadLabel(),
					enriched.Dst.PodUID,
					enriched.Dst.WorkloadKey(),
					enriched.CommText,
					enriched.Raw.Pid,
					enriched.Raw.Tid,
					enriched.Raw.LatencyUs,
					enriched.Raw.Ret,
					enriched.DropReasonName,
					enriched.DropCategory,
					enriched.Raw.Ifindex,
					enriched.Raw.SkbIif,
					enriched.Raw.SocketCookie,
					enriched.Raw.CgroupID,
				)
			}

		case err, ok := <-errCh:
			if ok && err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("ebpf runner error: %v", err)
			}
			errCh = nil

		case <-doneSignal:
			log.Printf("shutdown signal received")
			doneSignal = nil
		}
	}

	// 이벤트 루프가 끝난 뒤 (events/errCh 모두 close된 상태에서)
	// HTTP 서버를 graceful하게 종료한다. 루프 안에서 비동기로 처리하면
	// main이 먼저 return되어 Shutdown이 중단될 수 있어 여기서 동기 실행한다.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Printf("metrics server shutdown: %v", err)
	}

	log.Printf("exiting")
}
