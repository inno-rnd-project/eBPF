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
	"golang.org/x/sys/unix"

	"netobs/internal/kube"
	"netobs/internal/netobs/config"
	"netobs/internal/netobs/drop"
	ebpfx "netobs/internal/netobs/ebpf"
	"netobs/internal/netobs/flow"
	"netobs/internal/netobs/metadata"
	"netobs/internal/netobs/metrics"
	"netobs/internal/netobs/podbytes"
	"netobs/internal/netobs/selfhealth"
	"netobs/internal/netobs/symbols"
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
	// #83 의 drop stack 메트릭 cardinality 가드. allow-list 가 비어 있으면 emit 자체가 skip 되어
	// cardinality 0 series 로 유지된다.
	if len(cfg.DropStackAllowNamespaces) > 0 {
		metrics.SetDropStackGuard(metrics.NewDropStackGuard(cfg.DropStackAllowNamespaces, cfg.DropStackMaxActive))
		log.Printf("drop stack guard: allow_namespaces=%v max_active=%d", cfg.DropStackAllowNamespaces, cfg.DropStackMaxActive)
	} else {
		log.Printf("drop stack guard: disabled (NETOBS_DROP_STACK_ALLOW_NAMESPACES empty)")
	}
	// #142 drop 발생 시점 gauge (netobs_drop_last_timestamp_seconds) 의 monotonic→wall 변환 offset 을
	// startup 시 1회 산정한다. BPF bpf_ktime_get_ns 가 CLOCK_MONOTONIC 기준이라 (time.Now -
	// CLOCK_MONOTONIC) 차이를 더하면 ts_ns 가 unix epoch wall-clock 으로 환산된다. clock 읽기 실패 시
	// offset 미설정 (0) 으로 두면 metrics.Record 가 gauge Set 을 skip 해 monotonic 값을 wall-clock 으로
	// 오노출 하지 않는다. 즉 시점 gauge 만 비활성 되고 drop 추적 자체는 정상 동작한다. mono.Sec /
	// mono.Nsec 는 32-bit 아키텍처에서 int32 라 ns 환산 곱셈 전에 int64 로 승격해 오버플로를 막는다.
	var mono unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		log.Printf("drop timestamp clock offset: CLOCK_MONOTONIC read failed (%v); netobs_drop_last_timestamp_seconds disabled", err)
	} else {
		offsetNs := time.Now().UnixNano() - (int64(mono.Sec)*1_000_000_000 + int64(mono.Nsec))
		metrics.SetDropClockOffset(offsetNs)
		log.Printf("drop timestamp clock offset: %d ns (monotonic→wall)", offsetNs)
	}

	// #65 의 receive path TCP 상태 sample 을 수신 Pod 단위 gauge 로 노출하는 aggregator. Collector
	// 인터페이스를 직접 구현해 prometheus.Registerer 에 등록되며, Record 가 rcv_* stage event 의 TCP
	// 상태를 dispatch 하도록 setter 로 wire-up 한다.
	tcpStateAgg := metrics.NewTCPStateAggregator()
	reg.MustRegister(tcpStateAgg)
	metrics.SetTCPStateAggregator(tcpStateAgg)
	log.Printf("tcp state aggregator: registered (rcv_demux/rcv_established/rcv_app)")

	var ebpfReady atomic.Bool
	// dropStackResolver 는 onReady 시점에 생성되며 ebpfReady false 전환 시 Invalidate 가 호출되어
	// stale stack_id 매핑 회귀를 막는다. resolver init 실패 (kallsyms 미접근 등) 케이스는 nil 로 두어
	// metrics 패키지가 stack 메트릭 emit 만 fail-open 으로 skip 한다.
	var dropStackResolver atomic.Pointer[symbols.Resolver]
	kr := kube.NewResolver(cfg.NodeName, cfg.MetadataRefresh)
	enricher := metadata.NewEnricher(kr)

	// podbytes collector는 BPF의 pod_bytes 누적 맵을 scrape 시점에 iterate해 netobs_pod_bytes_total
	// 과 netobs_pod_packets_total을 emit한다. BPF가 준비되기 전 scrape는 빈 결과만 반환하며,
	// PodMetricsEnabled가 false면 어떤 시리즈도 emit하지 않는다 (enricher와 동일 토글 정합).
	podBytesCollector := podbytes.New(enricher, cfg.NodeName, cfg.PodMetricsEnabled)
	reg.MustRegister(podBytesCollector)

	// #85 flow.Collector 는 BPF flow_bytes 누적 맵 을 scrape 시점 에 iterate 해 netobs_flow_bytes_total
	// 5-tuple counter 를 emit 한다. FlowAllowNamespaces 가 비어 있으면 FlowGuard 의 모든 admit 이 거부
	// 되어 series 가 emit 되지 않는다 (opt-in 안전 default).
	var flowCollector *flow.Collector
	if len(cfg.FlowAllowNamespaces) > 0 {
		flowGuard := metrics.NewFlowGuard(cfg.FlowAllowNamespaces, cfg.FlowMaxActive)
		dstClassifier := metadata.NewDstLabelClassifier(cfg.PodFlowDstEnabled, cfg.PodFlowDstUIDAllowNamespaces)
		flowCollector = flow.New(enricher, kr, flowGuard, dstClassifier, cfg.NodeName, cfg.PodMetricsEnabled)
		reg.MustRegister(flowCollector)
		log.Printf("flow guard: allow_namespaces=%v max_active=%d", cfg.FlowAllowNamespaces, cfg.FlowMaxActive)
	} else {
		log.Printf("flow guard: disabled (NETOBS_FLOW_ALLOW_NAMESPACES empty)")
	}

	ready := func() (bool, string) {
		if !kr.HasSynced() {
			return false, "kube resolver informer not synced"
		}
		if !ebpfReady.Load() {
			return false, "ebpf not attached"
		}
		return true, ""
	}

	mux := server.NewMux("netobs-agent", reg, ready)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: mux,
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

	// #228 cgroup id 역매핑 스캐너. TCP 트래픽 없이 UDP 만 쓰는 pod 는 ringbuf 힌트가 학습되지 않아
	// cgroup 귀속이 실패하므로, informer 의 노드 pod 목록과 host cgroup2 inode 스캔으로 폴백 테이블을
	// 주기 재구성한다. host cgroup 마운트 부재 (로컬 실행 등) 시에는 테이블이 비어 기존 동작과 같다.
	if kr.Enabled() {
		scanner := metadata.NewCgroupScanner(kr, cfg.NodeName, metadata.DefaultCgroupRoot)
		enricher.SetCgroupScanner(scanner)
		go func() {
			// informer 첫 동기화를 기다린 뒤 스캔해야 기동 직후 빈 pod 목록으로 빈 테이블을 만들지
			// 않는다. Run 은 첫 스캔 직후 테이블 크기를 로그로 남긴다.
			for !kr.HasSynced() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(500 * time.Millisecond):
				}
			}
			scanner.Run(ctx, cfg.MetadataRefresh)
		}()
	}

	// informer sync lag emitter. 30s 주기로 lastWatchEvent 와 현재 시각의 차이를 self-health
	// gauge 로 노출한다. kube client 가 비활성 (in-cluster 와 KUBECONFIG 모두 부재) 인 local 환경
	// 에서는 lastWatchEvent 가 영원히 zero 라 fallback 이 단조 증가해 ObsAgentInformerStale 가
	// false positive 발화한다. Enabled() 가 false 면 emitter 자체를 spawn 하지 않아 시리즈가
	// 노출되지 않게 한다 (alert 의 informer_sync_lag 매칭이 자연 skip 된다).
	if kr.Enabled() {
		agentStartTime := time.Now()
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			// 첫 tick 전에도 한 번 emit 해 첫 scrape 시점에 0 이 아닌 값이 노출되도록 한다.
			emitInformerLag(agentStartTime, kr)
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					emitInformerLag(agentStartTime, kr)
				}
			}
		}()
	} else {
		log.Printf("informer sync lag emitter: skipped (kube resolver disabled)")
	}

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
				if flowCollector != nil {
					flowCollector.SetMap(rt.FlowBytes)
				}
				// #83 의 drop stack resolver. kallsyms 접근 실패 또는 권한 부족으로 init 이 실패하면
				// nil 로 두어 metrics 패키지가 stack 메트릭 emit 만 fail-open 으로 skip 한다.
				resolver, err := symbols.New(cfg.KallsymsPath, rt.DropStacks, 1024,
					metrics.IncDropStackResolverCacheHit, metrics.IncDropStackResolverCacheMiss)
				if err != nil {
					log.Printf("drop stack resolver: disabled (%v)", err)
				} else {
					dropStackResolver.Store(resolver)
					metrics.SetDropStackResolver(resolver)
					log.Printf("drop stack resolver: enabled (kallsyms=%s _text=%#x)", cfg.KallsymsPath, resolver.Base())
				}
				// self-health refresher 는 BPF map handle 이 준비된 시점에 한 번 spawn 한다.
				// 구성 실패는 self-health 만 disable 하고 agent 전체 기동은 진행해 운영자가
				// up{} 와 program_loaded 메트릭으로 1 차 진단을 시작할 수 있게 한다.
				if rf, err := selfhealth.NewRefresher(rt.Starts, rt.PodBytes, rt.EventsDropped, rt.DropStacks, rt.FlowBytes); err != nil {
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
			// invalidate한다. 이후 Collect는 nil-map 가드로 빈 결과를 반환한다. #85 flow.Collector
			// 도 동일한 stale FD 회피를 위해 함께 invalidate 한다.
			podBytesCollector.SetMap(nil)
			if flowCollector != nil {
				flowCollector.SetMap(nil)
			}
			// BPF runtime 이 종료된 시점에 readiness gate 도 false 로 reset 한다. 이렇게 두면
			// /readyz 가 즉시 503 을 돌려주어 Kubernetes Service endpoint 에서 제외되고, kubelet
			// 의 readiness probe 가 fail 후 Service 트래픽이 본 pod 으로 라우팅되지 않는다. BPF
			// 없는 좀비 상태로 stale 메트릭만 노출하는 자리를 막는다. #83 의 drop stack resolver
			// 도 BPF reload 시 stack_id 의미가 reset 되므로 함께 Invalidate 한다.
			ebpfReady.Store(false)
			if r := dropStackResolver.Load(); r != nil {
				r.Invalidate()
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

// emitInformerLag 는 kube.Resolver 의 마지막 watch event 시각과 현재 시각의 차이를 self-health
// gauge 로 emit 한다. zero (informer 미수신) 케이스에서는 agent 기동 시각으로 fallback 해 startup
// 직후 윈도우에서도 의미 있는 staleness 신호를 노출한다.
func emitInformerLag(startTime time.Time, kr *kube.Resolver) {
	last := kr.LastWatchEvent()
	if last.IsZero() {
		last = startTime
	}
	metrics.SetInformerSyncLag(time.Since(last).Seconds())
}
