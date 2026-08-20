// rca-summarizer 는 Alertmanager webhook 을 받아 alert 별 RCA 요약을 조합하는 long-running
// 프로세스다. /webhook 으로 수신한 발화 / 해결 이벤트를 mapping registry 로 dispatch 해 source
// 결과 (correlation-exporter snapshot, Prometheus instant query) 를 모으고 in-memory Store 와
// Prometheus metrics 에 반영한 뒤 /rca?alert=<name> JSON 응답과 /metrics 로 노출한다. webhook
// 응답 wall-clock 상한은 cfg.webhookTimeout 으로 http.Server WriteTimeout 에 전파된다.
//
// cluster 에 Deployment 단일 replica 로 배치된다. kube-prometheus-stack 의 Alertmanager 가
// AlertmanagerConfig 를 통해 /webhook 으로 알람을 보내고, kube-prometheus-stack 의
// ServiceMonitor 가 /metrics 를 scrape 한다.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	rcametrics "netobs/internal/rca/metrics"
	"netobs/internal/rca/registry"
	"netobs/internal/rca/server"
	"netobs/internal/rca/sources"
	"netobs/internal/rca/store"
	"netobs/internal/selfobs"
)

// config 는 -listen 등 binary 의 모든 운영 toggle 을 모은다. env fallback 은 flag 보다 후순위라
// CLI 가 항상 우선이다.
type config struct {
	listenAddr             string
	prometheusURL          string
	correlationSnapshotURL string
	webhookTimeout         time.Duration
	// confidenceThreshold 는 #122 의 multi-source cross-reference confidence score 의 false
	// positive guard threshold 다. RCASummary 의 ConfidenceScore 가 본 값 미만 인 alert 는
	// metrics emit 을 skip 하고 warn 로그 와 skipped_total counter 만 갱신 한다. 기본값 0.3 은
	// correlation 단일 신호 강도 가 0.6 이상 일 때 통과 가능 한 임계 (0.6 * WeightCorrelation
	// 0.5 = 0.3) 로 두어 다중 source cross-reference 부재 시 metrics 노이즈 를 차단 한다.
	confidenceThreshold float64
}

func parseConfig() (config, error) {
	cfg := config{
		listenAddr:             ":9850",
		prometheusURL:          "http://kube-prometheus-stack-prometheus.monitoring.svc:9090",
		correlationSnapshotURL: "http://correlation-exporter.ebpf-project.svc:9830/snapshot",
		webhookTimeout:         30 * time.Second,
		confidenceThreshold:    0.3,
	}

	if v := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); v != "" {
		cfg.listenAddr = v
	}
	if v := strings.TrimSpace(os.Getenv("PROMETHEUS_URL")); v != "" {
		cfg.prometheusURL = v
	}
	if v := strings.TrimSpace(os.Getenv("CORRELATION_SNAPSHOT_URL")); v != "" {
		cfg.correlationSnapshotURL = v
	}
	if v := strings.TrimSpace(os.Getenv("WEBHOOK_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			cfg.webhookTimeout = d
		}
	}
	if v := strings.TrimSpace(os.Getenv("RCA_CONFIDENCE_THRESHOLD")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			cfg.confidenceThreshold = f
		}
	}

	fs := flag.NewFlagSet("rca-summarizer", flag.ContinueOnError)
	fs.StringVar(&cfg.listenAddr, "listen", cfg.listenAddr, "metrics / webhook server listen address")
	fs.StringVar(&cfg.prometheusURL, "prometheus-url", cfg.prometheusURL, "Prometheus base URL for top-N instant query")
	fs.StringVar(&cfg.correlationSnapshotURL, "correlation-snapshot-url", cfg.correlationSnapshotURL, "correlation-exporter /snapshot URL")
	fs.DurationVar(&cfg.webhookTimeout, "webhook-timeout", cfg.webhookTimeout, "max wall-clock to compose RCA summary per webhook")
	fs.Float64Var(&cfg.confidenceThreshold, "confidence-threshold", cfg.confidenceThreshold, "#122: min ConfidenceScore to emit RCA metric (false positive guard)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return config{}, err
	}
	if cfg.webhookTimeout <= 0 {
		return config{}, errors.New("webhook-timeout must be positive")
	}
	if cfg.confidenceThreshold < 0 || cfg.confidenceThreshold > 1 {
		return config{}, errors.New("confidence-threshold must be in [0, 1]")
	}
	return cfg, nil
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("config: %v", err)
	}

	reg := prometheus.NewRegistry()
	// #405 프로세스 자기계측. Go runtime / process 표준 collector 와 cgroup limit 기반 GOMEMLIMIT.
	selfobs.RegisterProcessCollectors(reg)
	selfobs.ApplyMemoryLimit()
	var ready atomic.Bool

	rcaRegistry := registry.New()
	src := sources.New(cfg.correlationSnapshotURL, cfg.prometheusURL, 0, 0, 0)
	st := store.New()
	met := rcametrics.New()
	for _, c := range met.Collectors() {
		reg.MustRegister(c)
	}
	// #446 하류 신호 fetch 실패 카운터 (sources 패키지 소유).
	for _, c := range sources.Collectors() {
		reg.MustRegister(c)
	}

	mux := server.NewMux(server.Options{
		Registry: reg,
		Ready:    &ready,
		Webhook:  server.NewWebhookHandler(rcaRegistry, src, st, met, cfg.confidenceThreshold),
		RCA:      server.NewRCAHandler(st),
	})

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// webhookTimeout 을 ReadTimeout 과 WriteTimeout 에 전파해 wall-clock 상한을 강제한다.
		// 본 상한 안에서 webhook handler 가 registry.Dispatch 와 sources 호출 (snapshot HTTP,
		// Prometheus instant query) 을 모두 마쳐야 하며, 초과 시 클라이언트 (Alertmanager) 가
		// timeout 으로 인지해 다음 group_interval 에서 재발송 한다.
		ReadTimeout:  cfg.webhookTimeout,
		WriteTimeout: cfg.webhookTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("rca-summarizer listening on %s", cfg.listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	log.Printf("config: prometheus=%s snapshot=%s webhook_timeout=%s",
		cfg.prometheusURL, cfg.correlationSnapshotURL, cfg.webhookTimeout)

	// readiness 를 sources 초기 fetch 에 결합 한다. correlation-exporter snapshot 또는 Prometheus
	// query 가 연결 되면 ready 로 전환 하고, 실패 하면 not-ready 를 유지 한 채 짧은 backoff 로 재시도
	// 한다. webhook 수신 자체 는 ready 와 무관 하게 계속 serve 되어 sources 부재 시에도 degrade 동작
	// 한다. ctx 취소 (shutdown) 시 루프 가 종료 된다.
	go func() {
		const probeRetry = 5 * time.Second
		for {
			probeCtx, cancel := context.WithTimeout(ctx, sources.DefaultFetchTimeout)
			err := src.Probe(probeCtx)
			cancel()
			if err == nil {
				ready.Store(true)
				log.Printf("readiness: sources initial fetch ok, ready")
				return
			}
			log.Printf("readiness: sources initial fetch failed, webhook still served, retry in %s: %v", probeRetry, err)
			timer := time.NewTimer(probeRetry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()

	<-ctx.Done()
	log.Printf("shutdown: %v", ctx.Err())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
