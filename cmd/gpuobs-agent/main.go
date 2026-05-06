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
	"netobs/internal/gpuobs/metrics"
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

	// Kubernetes metadata informer (PodMetricsEnabled일 때만 기동).
	if kr != nil {
		go kr.Start(ctx)
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
		if nv == nil {
			log.Printf("warn: cuda uprobe enabled but NVML is unavailable; cuda reader skipped to avoid stale-series accumulation")
			// 기능이 켜져 있어도 NVML 의존성이 없으면 cuda goroutine 을 띄우지 않는다.
			// readyz 가 cuda 조건에서 영원히 막히지 않도록 ready 상태로 전환한다.
			cudaReady.Store(true)
		} else {
			cudaReader := cuda.New(cfg.CudaUprobeLibcudaPath, cfg.CudaUprobeLibcudartPath, cfg.NodeName, nv, resolver, cfg.CudaUprobeDeviceMapRefresh)
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
