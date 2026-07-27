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
	"netobs/internal/gpuobs/cuda"
	"netobs/internal/gpuobs/dcgm"
	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nccl"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/kube"
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
	// #104 gpuobs_pod_utilization_percent 의 namespace allow-list 통제 startup 적용. 빈 슬라이스 이면
	// 전체 발행 (기본값) 이고 명시 시 매칭 namespace 만 발행.
	metrics.SetPodUtilAllowNamespaces(cfg.PodUtilAllowNamespaces)
	if len(cfg.PodUtilAllowNamespaces) > 0 {
		log.Printf("pod util allow-list: %v (gpuobs_pod_utilization_percent emit restricted)", cfg.PodUtilAllowNamespaces)
	}
	// CUDA launch baseline tunable을 정적 gauge로 노출해 correlation host_compute_stall_score 의 분모
	// 로 사용한다.
	metrics.SetCudaLaunchBaselinePerSec(cfg.NodeName, cfg.CudaLaunchBaselinePerSec)
	log.Printf("cuda launch baseline: %.1f hz (node=%s)", cfg.CudaLaunchBaselinePerSec, cfg.NodeName)

	// #123/#133 DCGM source wire-up. 기본값 false로 dev cluster의 RTX 3090 환경에서는 noop
	// 구현이 wire-up되어 gpuobs_dcgm_available이 0 emit된다. GPUOBS_DCGM_ENABLED=true 이면 #133의
	// dcgm-exporter HTTP Source가 활성되어 cfg.DcgmExporterURL의 /metrics를 fetch한다. dcgm-
	// exporter가 부재한 환경에서는 Available이 false라 graceful degradation으로 0 emit된다.
	var dcgmSource dcgm.Source
	if cfg.DcgmEnabled {
		dcgmSource = dcgm.NewHTTPSource(cfg.DcgmExporterURL, 0)
		log.Printf("dcgm: HTTP source enabled (endpoint=%s)", cfg.DcgmExporterURL)
	} else {
		dcgmSource = dcgm.NewNoop()
	}
	defer func() { _ = dcgmSource.Close() }()
	metrics.SetDcgmAvailable(dcgmSource.Available())

	// #123/#134 NCCL profiler wire-up. 기본값 false로 dev cluster의 RTX 3090 환경에서는 noop이
	// wire-up되어 gpuobs_nccl_profiler_available이 0 emit된다. GPUOBS_NCCL_ENABLED=true이고 build
	// tag nccl로 빌드한 이미지이면 nccl.NewProduction이 cfg.NcclLibPath의 libnccl.so collective
	// 심볼에 uprobe를 attach하고 event 소비 goroutine이 collective duration을 metrics에 기록한다.
	// build tag nccl이 비활성인 기본 이미지에서는 NewProduction이 stub noop을 돌려주므로 Attach가
	// nil을 돌려줘도 Available이 false라 graceful degradation으로 0이 emit된다. attach 실패는
	// agent 기동을 막지 않고 noop으로 폴백한다.
	ncclProfiler := nccl.NewNoop()
	// #296 경로 자동 해석. env/flag 미지정이면 배포판별 후보를 순회하고, 전부 없으면 순회 목록을
	// 남기고 noop 으로 남는다 (기존 graceful degrade 유지).
	ncclLibPath := ""
	if cfg.NcclEnabled {
		ncclLibPath = config.ResolveLibPath(cfg.NcclLibPath, config.NcclLibCandidates())
		if ncclLibPath == "" {
			log.Printf("warn: libnccl.so.2 를 후보 경로에서 찾지 못해 nccl profiler 를 비활성한다 (순회: %v); GPUOBS_NCCL_LIB_PATH 로 고정하라", config.NcclLibCandidates())
		}
	}
	if cfg.NcclEnabled && ncclLibPath != "" {
		p := nccl.NewProduction(ncclLibPath, cfg.NodeName)
		if err := p.Attach(); err != nil {
			log.Printf("warn: nccl profiler attach failed: %v; falling back to noop", err)
			_ = p.Close()
		} else if p.Available() {
			ncclProfiler = p
			log.Printf("nccl: production profiler attached (libnccl=%s)", ncclLibPath)
			go func() {
				// Events 채널은 Close 시 닫혀 본 range가 정상 종료한다. duration_ns를 초로 변환해
				// gpuobs_nccl_collective_duration_seconds histogram에 누적한다.
				for ev := range ncclProfiler.Events() {
					metrics.RecordNcclCollective(cfg.NodeName, ev.Operation, float64(ev.DurationNs)/1e9)
				}
			}()
		} else {
			// build tag nccl이 비활성인 이미지의 stub noop. Attach는 nil이지만 Available이 false다.
			log.Printf("nccl: GPUOBS_NCCL_ENABLED=true but this image was built without the nccl build tag; falling back to noop")
			_ = p.Close()
		}
	}
	defer func() { _ = ncclProfiler.Close() }()
	metrics.SetNcclProfilerAvailable(ncclProfiler.Available())

	// kube.Resolver는 Pod/Service/Node informer를 띄우고 IP/UID 인덱스를 유지한다.
	// gpuobs는 ResolvePID 경로만 사용하므로, PodMetricsEnabled가 false인 경우에는
	// informer 자체를 기동하지 않아 RBAC/메모리 비용을 발생시키지 않고 /readyz도 kube sync에 묶이지 않는다.
	// 토글 on 시에는 pods/services/nodes 모두에 대해 RBAC가 필요하다 (deploy/gpuobs/base/clusterrole.yaml 참조).
	var kr *kube.Resolver
	if cfg.PodMetricsEnabled {
		kr = kube.NewResolver(cfg.NodeName, cfg.MetadataRefresh)
	}

	var collectorReady atomic.Bool
	var cudaReady atomic.Bool
	ready := func() (bool, string) {
		// kr이 nil이면 본 에이전트는 device-only 모드라 kube sync 의존이 없다.
		// kr이 주입된 경우에만 informer 동기화 완료를 readiness 조건으로 추가한다.
		if kr != nil && !kr.HasSynced() {
			return false, "kube resolver informer not synced"
		}
		if !collectorReady.Load() {
			return false, "collector not ready"
		}
		// cuda uprobe 가 활성일 때만 readiness 조건에 포함한다. 비활성 환경에서는 cuda goroutine 자체가
		// 기동되지 않으므로 조건에 끼우면 영원히 not-ready 상태가 된다.
		if cfg.CudaUprobeEnabled && !cudaReady.Load() {
			return false, "cuda uprobe reader not ready"
		}
		return true, ""
	}

	// Kubernetes metadata informer (PodMetricsEnabled일 때만 기동).
	if kr != nil {
		go kr.Start(ctx)
	}

	// informer sync lag emitter. kr 가 nil (PodMetricsEnabled=false) 인 device-only 모드 또는
	// kube client 가 비활성 (in-cluster 와 KUBECONFIG 모두 부재) 인 local 환경에서는 시리즈를
	// 노출하지 않는다. 30s 주기로 lastWatchEvent 와 현재 시각의 차이를 self-health gauge 로
	// 노출하고, 첫 이벤트 수신 전 윈도우에서는 agent 기동 시각으로 fallback 한다.
	if kr != nil && kr.Enabled() {
		agentStartTime := time.Now()
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
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

	// NVML 초기화는 수집이 활성화된 경우에만 시도한다. GPU_METRICS_ENABLED=false 환경에서는
	// libnvidia-ml.so.1 로드 자체를 건너뛰어 불필요한 초기화 비용과 실패 로그를 제거한다.
	// 활성화된 상태에서 non-GPU 노드나 driver 미설치로 Init이 실패하면 warn 로그만 남기고
	// nil 핸들을 주입해, collector가 graceful disable 경로로 분기하도록 한다.
	var nv nvml.NVML
	if cfg.GPUMetricsEnabled {
		if n, err := nvml.Init(); err != nil {
			log.Printf("warn: nvml init failed, gpuobs collector disabled: %v", err)
		} else {
			nv = n
		}
	}

	// kr이 nil 포인터일 때 그대로 PodResolver 인자로 넘기면 typed-nil interface가 되어
	// collector.pollOnce의 `c.resolver != nil` 검사가 true로 평가되며 잠재 nil deref가 발생한다.
	// 명시적으로 nil interface로 변환해 collector가 의도한 disable 분기로 떨어지게 한다.
	var resolver collector.PodResolver
	if kr != nil {
		resolver = kr
	}
	col := collector.New(nv, cfg, resolver)

	// HTTP 서버는 #281 의 /processes 핸들러가 collector 스냅샷에 묶여 있어 collector 생성 뒤에
	// 기동한다 (기동 후 mux 등록은 data race). ListenAndServe는 Shutdown 전까지 블록되는 것이
	// 정상 동작이며, 포트 바인드 실패 등 비정상 종료 시에는 fail-fast로 프로세스를 내려 메트릭
	// 없이 좀비 상태로 살아남는 상황을 막는다.
	mux := server.NewMux("gpuobs-agent", reg, ready)
	// #281 로컬 실행 프로세스 조회. 직전 poll 의 RunningProcesses 스냅샷 재사용이라 NVML 추가
	// 호출이 없다. correlation-exporter 의 gpu-processes 프록시가 단일 진입점으로 노출한다.
	mux.HandleFunc("/processes", col.ProcessesHandler())

	// #357 http.Server 타임아웃 완비 (rca-summarizer 동형). 타임아웃 부재 시 slow-header / slow-body /
	// slow-consumer 커넥션이 goroutine 과 fd 를 무기한 점유해 노드별 관측 에이전트를 소량 커넥션으로
	// 고갈시킬 수 있다. WriteTimeout 30s 는 Prometheus scrape_timeout (기본 10s) 의 3 배 여유로 정상
	// scrape 와 /processes 스냅샷을 절단하지 않으면서 slow-consumer 를 상한한다. IdleTimeout 120s 는
	// scrape 간격 (15~30s) 보다 커 keep-alive 커넥션 재사용을 보존하면서 유휴 커넥션을 회수한다.
	// gpuobs 는 range fetch 가 없어 WriteTimeout 을 고정 30s 로 둔다 (correlation 은 FetchTimeout 에
	// 연동해 동적, #357/#360).
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("serving gpuobs metrics on %s (node=%s)", cfg.ListenAddr, cfg.NodeName)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("metrics server error: %v", err)
		}
	}()

	collectorErrCh := make(chan error, 1)
	go func() {
		collectorErrCh <- col.Run(ctx, func() {
			collectorReady.Store(true)
		})
		close(collectorErrCh)
	}()

	// cuda uprobe 모듈은 별도 goroutine 으로 운영한다. CudaUprobeEnabled=false 환경에서는 BPF 객체 로드 /
	// libcuda hostPath 의존성 / CAP_BPF 등의 capability 요구를 피하기 위해 인스턴스화 자체를 건너뛴다.
	//
	// nv==nil + cuda enabled 시: cuda 패키지의 deviceMap refresher 가 즉시 종료해 RetainCudaSeries 가 영원히
	// 호출되지 않는다. 그러면 종료된 Pod 의 cuda counter 시리즈와 seenCudaKeys 가 무제한 누적되어 카디널리티
	// 폭증 / 메모리 누수를 유발한다. 이를 막기 위해 cuda reader 자체를 시작하지 않고 warn 로깅 + ready 처리만 한다.
	var cudaErrCh chan error
	if cfg.CudaUprobeEnabled {
		// #296 경로 자동 해석. env/flag 미지정이면 배포판별 후보를 순회하고, 전부 없으면 순회
		// 목록을 남기고 reader 를 띄우지 않는다 (기존 libcuda 부재와 동일한 graceful 비활성).
		libcudaPath := config.ResolveLibPath(cfg.CudaUprobeLibcudaPath, config.LibcudaCandidates())
		if nv == nil {
			log.Printf("warn: cuda uprobe enabled but NVML is unavailable; cuda reader skipped to avoid stale-series accumulation")
			// 기능이 켜져 있어도 NVML 의존성이 없으면 cuda goroutine 을 띄우지 않는다.
			// readyz 가 cuda 조건에서 영원히 막히지 않도록 ready 상태로 전환한다.
			cudaReady.Store(true)
		} else if libcudaPath == "" {
			log.Printf("warn: libcuda.so.1 을 후보 경로에서 찾지 못해 cuda uprobe 를 비활성한다 (순회: %v); GPUOBS_CUDA_LIBCUDA_PATH 로 고정하라", config.LibcudaCandidates())
			cudaReady.Store(true)
		} else {
			log.Printf("cuda: libcuda resolved (%s)", libcudaPath)
			cudaReader := cuda.New(libcudaPath, cfg.CudaUprobeLibcudartPath, cfg.NodeName, nv, resolver, cfg.CudaUprobeDeviceMapRefresh)
			cudaErrCh = make(chan error, 1)
			go func() {
				err := cudaReader.Run(ctx, func() {
					cudaReady.Store(true)
				})
				// cuda 가 onReady 를 호출하기 전에 에러로 빠진 경우(libcuda 미존재 / 모든 심볼 attach 실패 등)
				// readyz 가 영원히 not-ready 로 묶이는 것을 막기 위해, goroutine 종료 시점에 cudaReady 를 강제 set 한다.
				// 진단 정보는 errCh 로깅 + gpuobs_cuda_symbol_available 메트릭으로 운영자가 확인한다.
				cudaReady.Store(true)
				cudaErrCh <- err
				close(cudaErrCh)
			}()
		}
	}

	// 이벤트 루프.
	// ctx가 취소되면 doneSignal을 nil로 돌려 해당 case를 비활성화한다.
	// 각 errCh 가 close되면 채널 변수를 nil 로 만들어 select 에서 영원히 블록 → 자연스럽게 drain 된다.
	doneSignal := ctx.Done()
	for collectorErrCh != nil || cudaErrCh != nil {
		select {
		case err, ok := <-collectorErrCh:
			if ok && err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("collector error: %v", err)
			}
			collectorErrCh = nil
		case err, ok := <-cudaErrCh:
			if ok && err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("cuda reader error: %v", err)
			}
			cudaErrCh = nil
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

// emitInformerLag 는 kube.Resolver 의 마지막 watch event 시각과 현재 시각의 차이를 self-health
// gauge 로 emit 한다. zero (informer 미수신) 케이스에서는 agent 기동 시각으로 fallback 해 startup
// 직후 윈도우에서도 의미 있는 staleness 신호를 노출한다. netobs main 의 동명 헬퍼와 동일 의미.
func emitInformerLag(startTime time.Time, kr *kube.Resolver) {
	last := kr.LastWatchEvent()
	if last.IsZero() {
		last = startTime
	}
	metrics.SetInformerSyncLag(time.Since(last).Seconds())
}
