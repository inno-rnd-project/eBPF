// rca-summarizer 는 Alertmanager webhook 을 받아 발화 alert 의 root cause analysis 요약을
// 30 초 안에 산출해 /rca endpoint 로 노출하는 long-running 프로세스다. mapping registry 와 Top-N
// source 는 후속 commit 에서 채워지며 본 binary 의 책임은 HTTP server lifecycle 과 config 관리다.
//
// cluster 에 Deployment 단일 replica 로 배치된다. kube-prometheus-stack 의 Alertmanager 가
// AlertmanagerConfig 를 통해 /webhook 으로 발화 / 해결 알람을 보내고, kube-prometheus-stack 의
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
)

// config 는 -listen 등 binary 의 모든 운영 toggle 을 모은다. env fallback 은 flag 보다 후순위라
// CLI 가 항상 우선이다.
type config struct {
	listenAddr             string
	prometheusURL          string
	correlationSnapshotURL string
	webhookTimeout         time.Duration
}

func parseConfig() (config, error) {
	cfg := config{
		listenAddr:             ":9850",
		prometheusURL:          "http://kube-prometheus-stack-prometheus.monitoring.svc:9090",
		correlationSnapshotURL: "http://correlation-exporter.ebpf-project.svc:9830/snapshot",
		webhookTimeout:         30 * time.Second,
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

	fs := flag.NewFlagSet("rca-summarizer", flag.ContinueOnError)
	fs.StringVar(&cfg.listenAddr, "listen", cfg.listenAddr, "metrics / webhook server listen address")
	fs.StringVar(&cfg.prometheusURL, "prometheus-url", cfg.prometheusURL, "Prometheus base URL for top-N instant query")
	fs.StringVar(&cfg.correlationSnapshotURL, "correlation-snapshot-url", cfg.correlationSnapshotURL, "correlation-exporter /snapshot URL")
	fs.DurationVar(&cfg.webhookTimeout, "webhook-timeout", cfg.webhookTimeout, "max wall-clock to compose RCA summary per webhook")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return config{}, err
	}
	if cfg.webhookTimeout <= 0 {
		return config{}, errors.New("webhook-timeout must be positive")
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
	var ready atomic.Bool

	rcaRegistry := registry.New()
	src := sources.New(cfg.correlationSnapshotURL, cfg.prometheusURL, 0, 0, 0)
	st := store.New()
	met := rcametrics.New()
	for _, c := range met.Collectors() {
		reg.MustRegister(c)
	}

	mux := server.NewMux(server.Options{
		Registry: reg,
		Ready:    &ready,
		Webhook:  server.NewWebhookHandler(rcaRegistry, src, st, met),
		RCA:      server.NewRCAHandler(st),
	})

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
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

	// skeleton 단계: 후속 commit 이 sources 초기 fetch 성공을 readiness 조건에 묶기 전까지
	// 즉시 ready 로 둔다. /metrics 와 /healthz 만 사용해도 의미 있는 baseline 검증이 가능하다.
	ready.Store(true)

	<-ctx.Done()
	log.Printf("shutdown: %v", ctx.Err())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
