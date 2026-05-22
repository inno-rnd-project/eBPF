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

	"netobs/internal/kube"
	"netobs/internal/netobs/config"
	"netobs/internal/netobs/drop"
	ebpfx "netobs/internal/netobs/ebpf"
	"netobs/internal/netobs/metadata"
	"netobs/internal/netobs/metrics"
	"netobs/internal/netobs/podbytes"
	"netobs/internal/netobs/selfhealth"
	"netobs/internal/netobs/types"
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
	metrics.SetPodMetricsEnabled(cfg.PodMetricsEnabled)
	// dst 라벨 정책 단일 진입점. master switch off / allow-list 비어 있으면 dst_namespace, dst_workload,
	// dst_pod_uid 가 빈 문자열로 emit 되어 cardinality 가 도입 전 수준으로 유지된다.
	metrics.SetDstClassifier(metadata.NewDstLabelClassifier(cfg.PodFlowDstEnabled, cfg.PodFlowDstUIDAllowNamespaces))
	log.Printf("pod flow dst labels: enabled=%t dst_pod_uid_allow_namespaces=%v", cfg.PodFlowDstEnabled, cfg.PodFlowDstUIDAllowNamespaces)
	// 노드 NIC capacity tunable을 정적 gauge로 노출해 correlation recording rule이 분모로 사용한다.
	metrics.SetNICCapacityBytesPerSec(cfg.NodeName, cfg.NICCapacityBytesPerSec)
	log.Printf("nic capacity: %.0f bytes/sec (node=%s)", cfg.NICCapacityBytesPerSec, cfg.NodeName)
	// #64 의 drop flow 5-tuple 메트릭 cardinality 가드. allow-list 가 비어 있으면 emit 자체가 skip
	// 되어 카디널리티 0 series 로 유지된다. 운영자가 진단 대상 namespace 만 명시 등록.
	if len(cfg.DropFlowAllowNamespaces) > 0 {
		metrics.SetDropFlowGuard(metrics.NewDropFlowGuard(cfg.DropFlowAllowNamespaces, cfg.DropFlowMaxActive))
		log.Printf("drop flow guard: allow_namespaces=%v max_active=%d", cfg.DropFlowAllowNamespaces, cfg.DropFlowMaxActive)
	} else {
		log.Printf("drop flow guard: disabled (NETOBS_DROP_FLOW_ALLOW_NAMESPACES empty)")
	}

	// #65 의 receive path TCP 상태 sample 을 수신 Pod 단위 gauge 로 노출하는 aggregator. Collector
	// 인터페이스를 직접 구현해 prometheus.Registerer 에 등록되며, Record 가 rcv_* stage event 의 TCP
	// 상태를 dispatch 하도록 setter 로 wire-up 한다.
	tcpStateAgg := metrics.NewTCPStateAggregator()
	reg.MustRegister(tcpStateAgg)
	metrics.SetTCPStateAggregator(tcpStateAgg)
	log.Printf("tcp state aggregator: registered (rcv_demux/rcv_established/rcv_app)")

	var ebpfReady atomic.Bool
	kr := kube.NewResolver(cfg.NodeName, cfg.MetadataRefresh)
	enricher := metadata.NewEnricher(kr)

	// podbytes collector는 BPF의 pod_bytes 누적 맵을 scrape 시점에 iterate해 netobs_pod_bytes_total
	// 과 netobs_pod_packets_total을 emit한다. BPF가 준비되기 전 scrape는 빈 결과만 반환하며,
	// PodMetricsEnabled가 false면 어떤 시리즈도 emit하지 않는다 (enricher와 동일 토글 정합).
	podBytesCollector := podbytes.New(enricher, cfg.NodeName, cfg.PodMetricsEnabled)
	reg.MustRegister(podBytesCollector)

	ready := func() (bool, string) {
		if !kr.HasSynced() {
			return false, "kube resolver informer not synced"
		}
		if !ebpfReady.Load() {
			return false, "ebpf not attached"
		}
		return true, ""
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server.NewHandler("netobs-agent", reg, ready),
	}

	// HTTP server: ListenAndServe는 shutdown 전까지 블록되어야 정상 동작이며,
	// 포트 바인드 실패 등 비정상 종료 시에는 fail-fast로 프로세스를 내려 메트릭 없이
	// 좀비 상태로 살아남는 상황을 막는다.
	go func() {
		log.Printf("serving metrics on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics server error: %v", err)
		}
	}()

	// Kubernetes metadata informer.
	go kr.Start(ctx)

	mapper := drop.NewMapper(drop.DefaultPaths(cfg.DropReasonFormatPath))

	// eBPF runner. ctx가 취소되면 내부에서 ringbuf를 닫고 events 채널을 close한 뒤
	// 최종 에러를 errCh로 전달한다.
	events := make(chan types.Event, 4096)
	errCh := make(chan error, 1)

	go func() {
		errCh <- ebpfx.Run(ctx, cfg.TargetIP, events, func(rt *ebpfx.Runtime) {
			ebpfReady.Store(true)
			if rt != nil {
				podBytesCollector.SetMap(rt.PodBytes)
				// self-health refresher 는 BPF map handle 이 준비된 시점에 한 번 spawn 한다.
				// 구성 실패는 self-health 만 disable 하고 agent 전체 기동은 진행해 운영자가
				// up{} 와 program_loaded 메트릭으로 1 차 진단을 시작할 수 있게 한다.
				if rf, err := selfhealth.NewRefresher(rt.Starts, rt.PodBytes, rt.EventsDropped); err != nil {
					log.Printf("self-health refresher: %v", err)
				} else {
					rf.Start(ctx)
					log.Printf("self-health refresher: started (interval=%s)", selfhealth.DefaultRefreshInterval)
				}
			}
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

			enriched := enricher.Enrich(ev, mapper)
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
			// ebpfx.Run 반환 시점에 deferred objs.Close()가 이미 PodBytes 맵 FD를 닫았다.
			// Collector가 stale pointer로 매 scrape마다 EBADF errno를 받지 않도록 명시적으로
			// invalidate한다. 이후 Collect는 nil-map 가드로 빈 결과를 반환한다.
			podBytesCollector.SetMap(nil)
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
