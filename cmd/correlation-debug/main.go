// correlation-debug 는 internal/correlation 라이브러리의 일회성 검증 CLI 다. 운영자가 GPU 유휴
// alert 발화 후 직전 1시간 윈도우로 어떤 Pod 페어가 강한 상관을 보이는지 확인하는 흐름에 쓰인다.
// 본 binary 는 cluster 에 배포되지 않으며 운영자가 로컬에서 build / 실행 (port-forward Prometheus
// 접근) 한다. 주기적 자동화는 #51 (Top-N noisy neighbor exporter) 가 다룬다.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"netobs/internal/correlation"
)

// stringSlice 는 -extra-metric 처럼 반복 가능한 flag 를 위한 flag.Value 구현이다.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// intSlice 는 -lag-steps 처럼 콤마 구분 int 슬라이스를 받는 flag.Value 구현이다.
type intSlice []int

func (s *intSlice) String() string {
	parts := make([]string, len(*s))
	for i, v := range *s {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

func (s *intSlice) Set(v string) error {
	*s = (*s)[:0]
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		n, err := strconv.Atoi(tok)
		if err != nil {
			return fmt.Errorf("invalid int in lag-steps %q: %w", tok, err)
		}
		*s = append(*s, n)
	}
	return nil
}

const (
	exitOK            = 0
	exitFetchFailure  = 1
	exitFlagError     = 2
	exitEncodeFailure = 3
)

func main() {
	cfg := correlation.DefaultConfig()

	// PROMETHEUS_URL env fallback 으로 default 보다 환경 변수가 우선하고 CLI flag 가 최우선.
	if v := strings.TrimSpace(os.Getenv("PROMETHEUS_URL")); v != "" {
		cfg.PrometheusURL = v
	}

	fs := flag.NewFlagSet("correlation-debug", flag.ContinueOnError)
	fs.StringVar(&cfg.PrometheusURL, "prometheus-url", cfg.PrometheusURL, "Prometheus base URL (env PROMETHEUS_URL fallback)")
	fs.DurationVar(&cfg.Window, "window", cfg.Window, "query_range window (e.g., 1h, 24h)")
	fs.DurationVar(&cfg.Step, "step", cfg.Step, "query_range step (matches Prometheus scrape interval typically)")
	fs.IntVar(&cfg.MinSamples, "min-samples", cfg.MinSamples, "minimum valid samples per pair after NaN/Inf removal; below threshold pairs are skipped")
	fs.DurationVar(&cfg.FetchTimeout, "fetch-timeout", cfg.FetchTimeout, "HTTP timeout for each query_range call")

	var extra stringSlice
	fs.Var(&extra, "extra-metric", "additional Prometheus query to include (repeat for multiple)")

	var lagSteps intSlice
	for _, l := range cfg.LagSteps {
		lagSteps = append(lagSteps, l)
	}
	fs.Var(&lagSteps, "lag-steps", "comma-separated lag steps (e.g., -1,0,1); a positive lag k means corr(a[t], b[t+k])")

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(exitOK)
		}
		fmt.Fprintf(os.Stderr, "flag error: %v\n", err)
		os.Exit(exitFlagError)
	}
	cfg.ExtraMetrics = []string(extra)
	cfg.LagSteps = []int(lagSteps)

	if cfg.Window <= 0 || cfg.Step <= 0 {
		fmt.Fprintln(os.Stderr, "window and step must be positive")
		os.Exit(exitFlagError)
	}
	if cfg.FetchTimeout <= 0 {
		fmt.Fprintln(os.Stderr, "fetch-timeout must be positive (zero or negative yields immediate context expiry)")
		os.Exit(exitFlagError)
	}
	if cfg.MinSamples <= 0 {
		fmt.Fprintln(os.Stderr, "min-samples must be positive")
		os.Exit(exitFlagError)
	}
	if len(cfg.LagSteps) == 0 {
		fmt.Fprintln(os.Stderr, "lag-steps must not be empty (at least one lag step required)")
		os.Exit(exitFlagError)
	}

	fetcher := correlation.NewPrometheusFetcher(cfg.PrometheusURL, cfg.FetchTimeout)
	corr := correlation.New(fetcher, cfg)

	log.Printf("correlating: prometheus=%s window=%s step=%s lag_steps=%v min_samples=%d",
		cfg.PrometheusURL, cfg.Window, cfg.Step, cfg.LagSteps, cfg.MinSamples)
	log.Printf("metrics: default=%d extra=%d", len(cfg.DefaultMetrics), len(cfg.ExtraMetrics))

	ctx, cancel := context.WithTimeout(context.Background(), cfg.FetchTimeout*time.Duration(len(cfg.DefaultMetrics)+len(cfg.ExtraMetrics)+1))
	defer cancel()

	// CLI 는 \"지금 기준 직전 Window\" 가 자연스러운 의도라 time.Now() 를 명시 인자로 넘긴다. 과거
// 시점 분석이 필요하면 운영자가 라이브러리를 직접 호출하거나 본 CLI 를 future fork 로 확장한다.
	results, err := corr.Correlate(ctx, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "correlation failed: %v\n", err)
		os.Exit(exitFetchFailure)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(results); err != nil {
		fmt.Fprintf(os.Stderr, "json encode failed: %v\n", err)
		os.Exit(exitEncodeFailure)
	}
	log.Printf("emitted %d correlation results", len(results))
}
