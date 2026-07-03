// correlation-exporter 는 internal/correlation 라이브러리의 산출물을 주기적으로 갱신해 Prometheus
// 메트릭으로 노출하는 long-running 프로세스다. cluster 에 Deployment 단일 replica 로 배치되며
// kube-prometheus-stack 의 ServiceMonitor 가 /metrics 를 scrape 한다.
//
// 본 binary 는 cluster API 권한이 필요 없으며 Prometheus HTTP API 만 호출한다. correlation-debug
// CLI 와 동일 라이브러리를 reuse 하므로 동일 endTime 기준의 산출 결과는 두 도구에서 일치한다.

// swaggo general info. 아래 @tag 선언은 REST API 19 종의 기능 도메인 7 분류로, 각 핸들러의
// @Tags 값과 1:1 정합해야 swagger UI 그룹 헤더에 설명이 노출된다. 태그 추가 시 본 선언과
// 핸들러 @Tags, docs/api/coverage.md 의 분류 표를 함께 갱신한다.

// @title        netobs correlation-exporter API
// @version      1.0
// @description  eBPF 네트워크/GPU 관측 신호를 합성해 노출하는 진단 REST API. 모든 엔드포인트는 GET 이며 Prometheus 를 단일 데이터 소스로 쓴다.
// @BasePath     /

// @tag.name         meta
// @tag.description  API 상태와 클러스터 헬스 요약
// @tag.name         inventory
// @tag.description  노드와 파드 인벤토리 (k8s 오브젝트 스냅샷)
// @tag.name         network
// @tag.description  네트워크 지연 단계 분해와 패킷 drop, pod 간 flow
// @tag.name         interference
// @tag.description  자원 압박 랭킹과 노드 상세, 이벤트, 메모리 병목, 간섭 토폴로지
// @tag.name         impact
// @tag.description  간섭 상관 top-N 과 영향 전파 그래프 (noisy neighbor, cross-node, service impact)
// @tag.name         gpu
// @tag.description  GPU 유휴 원인 분석
// @tag.name         trends
// @tag.description  진단 신호 시계열 추이
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	"github.com/prometheus/client_golang/prometheus/promhttp"
	httpSwagger "github.com/swaggo/http-swagger/v2"

	"netobs/internal/correlation"
	"netobs/internal/correlation/api"
	correlationdocs "netobs/internal/correlation/api/docs"
	"netobs/internal/correlation/exporter"
)

// maxTopN 은 -top-n flag 의 상한이다. victim 1k * dimension 4 * rank 100 = gauge 당 400k series
// 가 절대 상한이며 noisy-neighbor 결과는 neighbor 마다 score / lag gauge 두 종을 같은 라벨 셋으로
// 함께 emit 하므로 본 두 메트릭만 합쳐도 약 800k series 까지 갈 수 있다. 운영자가 의도치 않게
// series 폭주를 일으키지 않도록 binary 단에서 본 상한으로 가드한다.
const maxTopN = 100

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

func main() {
	cfg := correlation.DefaultConfig()
	reconcileInterval := 5 * time.Minute
	listenAddr := ":9830"
	topN := 10

	// env fallback. flag 가 우선이라 후순위로 적용.
	if v := strings.TrimSpace(os.Getenv("PROMETHEUS_URL")); v != "" {
		cfg.PrometheusURL = v
	}
	applyEnvDuration("WINDOW", "window", &cfg.Window)
	applyEnvDuration("STEP", "step", &cfg.Step)
	applyEnvDuration("FETCH_TIMEOUT", "fetch-timeout", &cfg.FetchTimeout)
	applyEnvDuration("RECONCILE_INTERVAL", "reconcile-interval", &reconcileInterval)
	applyEnvInt("MIN_SAMPLES", "min-samples", &cfg.MinSamples)
	applyEnvInt("TOP_N", "top-n", &topN)
	// #151 Phase 2 경로 추출 tunable 의 env override. 조밀 그래프에서 IMPACT_PATH_MIN_SCORE 를 올려
	// 약한 엣지를 더 쳐내면 근원 (root) 이 드러나 경로가 추출된다.
	applyEnvInt("IMPACT_PATH_MAX_DEPTH", "impact-path-max-depth", &cfg.ImpactPathMaxDepth)
	applyEnvFloat("IMPACT_PATH_MIN_SCORE", "impact-path-min-score", &cfg.ImpactPathMinScore)
	applyEnvInt("IMPACT_PATH_MAX_PATHS", "impact-path-max-paths", &cfg.ImpactPathMaxPaths)
	if v := strings.TrimSpace(os.Getenv("LAG_STEPS")); v != "" {
		var parsed intSlice
		if err := parsed.Set(v); err != nil {
			if !hasCLIFlag(os.Args[1:], "lag-steps") {
				log.Fatalf("env LAG_STEPS parse: %v", err)
			}
		} else {
			cfg.LagSteps = []int(parsed)
		}
	}
	if v := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); v != "" {
		listenAddr = v
	}
	// #84/#147 cross-node interference layer 의 토글. #147 부터 default 활성 (config.DefaultConfig)
	// 이며 CROSS_NODE 환경변수 로 양방향 override 한다. 값이 있으면 strconv.ParseBool 로 해석 해
	// "1"/"true" 는 활성, "0"/"false" 는 opt-out (cardinality 부담 환경) 으로 둔다. parse 실패 값은
	// default 를 유지 하고 warn 로깅 한다. 단 -cross-node flag 가 제공되면 그 값이 env 를 덮어쓰므로
	// warn 을 생략 해 applyEnvDuration / applyEnvInt / LAG_STEPS 와 동일 정책 을 따른다.
	if v := strings.TrimSpace(os.Getenv("CROSS_NODE")); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.CrossNodeEnabled = parsed
		} else if !hasCLIFlag(os.Args[1:], "cross-node") {
			log.Printf("warn: invalid CROSS_NODE=%q; using default %v", v, cfg.CrossNodeEnabled)
		}
	}
	// #148 service-impact layer 의 토글. default 활성 (config.DefaultConfig) 이며 SERVICE_IMPACT 환경
	// 변수로 양방향 override 한다. CROSS_NODE 와 동일하게 strconv.ParseBool 로 해석하고 parse 실패 값은
	// default 를 유지한다. -service-impact flag 가 제공되면 그 값이 env 를 덮어쓰므로 warn 을 생략한다.
	if v := strings.TrimSpace(os.Getenv("SERVICE_IMPACT")); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.ServiceImpactEnabled = parsed
		} else if !hasCLIFlag(os.Args[1:], "service-impact") {
			log.Printf("warn: invalid SERVICE_IMPACT=%q; using default %v", v, cfg.ServiceImpactEnabled)
		}
	}
	// #149 cross-level layer 의 토글. default 활성 (config.DefaultConfig) 이며 CROSS_LEVEL 환경변수로
	// 양방향 override 한다. CROSS_NODE / SERVICE_IMPACT 와 동일하게 strconv.ParseBool 로 해석하고 parse
	// 실패 값은 default 를 유지한다. -cross-level flag 가 제공되면 그 값이 env 를 덮어쓰므로 warn 을 생략한다.
	if v := strings.TrimSpace(os.Getenv("CROSS_LEVEL")); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.CrossLevelEnabled = parsed
		} else if !hasCLIFlag(os.Args[1:], "cross-level") {
			log.Printf("warn: invalid CROSS_LEVEL=%q; using default %v", v, cfg.CrossLevelEnabled)
		}
	}
	// #151 Phase 1 영향 전파 그래프 토글. default 활성 (config.DefaultConfig) 이며 IMPACT_GRAPH 환경
	// 변수로 양방향 override 한다. 다른 토글과 동일하게 strconv.ParseBool 로 해석하고 parse 실패 값은
	// default 를 유지한다. -impact-graph flag 가 제공되면 그 값이 env 를 덮어쓰므로 warn 을 생략한다.
	if v := strings.TrimSpace(os.Getenv("IMPACT_GRAPH")); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			cfg.ImpactGraphEnabled = parsed
		} else if !hasCLIFlag(os.Args[1:], "impact-graph") {
			log.Printf("warn: invalid IMPACT_GRAPH=%q; using default %v", v, cfg.ImpactGraphEnabled)
		}
	}

	fs := flag.NewFlagSet("correlation-exporter", flag.ContinueOnError)
	fs.StringVar(&cfg.PrometheusURL, "prometheus-url", cfg.PrometheusURL, "Prometheus base URL (env PROMETHEUS_URL fallback)")
	fs.DurationVar(&cfg.Window, "window", cfg.Window, "query_range window")
	fs.DurationVar(&cfg.Step, "step", cfg.Step, "query_range step")
	fs.IntVar(&cfg.MinSamples, "min-samples", cfg.MinSamples, "minimum valid samples per pair after NaN/Inf removal")
	fs.DurationVar(&cfg.FetchTimeout, "fetch-timeout", cfg.FetchTimeout, "HTTP timeout for each query_range call")
	fs.DurationVar(&reconcileInterval, "reconcile-interval", reconcileInterval, "interval between reconcile cycles")
	fs.StringVar(&listenAddr, "listen", listenAddr, "metrics server listen address")
	fs.IntVar(&topN, "top-n", topN, fmt.Sprintf("Top-N noisy neighbors per (victim, dimension), max %d", maxTopN))
	fs.BoolVar(&cfg.CrossNodeEnabled, "cross-node", cfg.CrossNodeEnabled, "#84/#147: cross-node interference layer (node-level pair enumeration, correlation_cross_node_score gauge). Default enabled; set -cross-node=false or CROSS_NODE=false to opt out on very large clusters")
	fs.BoolVar(&cfg.ServiceImpactEnabled, "service-impact", cfg.ServiceImpactEnabled, "#148: service-impact layer (node pressure vs workload-level latency, correlation_service_impact_score gauge). Default enabled; set -service-impact=false or SERVICE_IMPACT=false to opt out on very large clusters")
	fs.BoolVar(&cfg.CrossLevelEnabled, "cross-level", cfg.CrossLevelEnabled, "#149: cross-level layer (same-node node pressure vs pod latency, both directions, correlation_cross_level_score gauge). Default enabled; set -cross-level=false or CROSS_LEVEL=false to opt out on very large clusters")
	fs.BoolVar(&cfg.ImpactGraphEnabled, "impact-graph", cfg.ImpactGraphEnabled, "#151 Phase 1: in-memory impact propagation graph (nodes=pods, edges=suspect->victim, correlation_impact_graph_node_degree gauge + /api/v1/impact-graph). Default enabled; set -impact-graph=false or IMPACT_GRAPH=false to opt out")
	fs.IntVar(&cfg.ImpactPathMaxDepth, "impact-path-max-depth", cfg.ImpactPathMaxDepth, "#151 Phase 2: max hop depth for impact path extraction (env IMPACT_PATH_MAX_DEPTH)")
	fs.Float64Var(&cfg.ImpactPathMinScore, "impact-path-min-score", cfg.ImpactPathMinScore, "#151 Phase 2: min edge score to include in impact paths; raise on dense graphs to surface roots (env IMPACT_PATH_MIN_SCORE)")
	fs.IntVar(&cfg.ImpactPathMaxPaths, "impact-path-max-paths", cfg.ImpactPathMaxPaths, "#151 Phase 2: max extracted impact paths (combinatorial backstop, env IMPACT_PATH_MAX_PATHS)")

	var extra stringSlice
	fs.Var(&extra, "extra-metric", "additional Prometheus query (repeat for multiple)")

	var lagSteps intSlice
	for _, l := range cfg.LagSteps {
		lagSteps = append(lagSteps, l)
	}
	fs.Var(&lagSteps, "lag-steps", "comma-separated lag steps (e.g., -1,0,1)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("flag error: %v", err)
	}
	cfg.ExtraMetrics = []string(extra)
	cfg.LagSteps = []int(lagSteps)

	if cfg.Window <= 0 || cfg.Step <= 0 {
		log.Fatalf("window and step must be positive")
	}
	if cfg.FetchTimeout <= 0 {
		log.Fatalf("fetch-timeout must be positive")
	}
	if cfg.MinSamples <= 0 {
		log.Fatalf("min-samples must be positive")
	}
	if reconcileInterval <= 0 {
		log.Fatalf("reconcile-interval must be positive")
	}
	if topN <= 0 || topN > maxTopN {
		log.Fatalf("top-n must be in [1, %d]", maxTopN)
	}
	if len(cfg.LagSteps) == 0 {
		log.Fatalf("lag-steps must not be empty")
	}

	fetcher, err := correlation.NewPrometheusFetcher(cfg.PrometheusURL, cfg.FetchTimeout)
	if err != nil {
		log.Fatalf("fetcher init: %v", err)
	}
	corr := correlation.New(fetcher, cfg)

	reg := prometheus.NewRegistry()
	collector := exporter.NewCollector(cfg.Step)
	reg.MustRegister(collector)
	health := exporter.NewHealth(reg)

	var ready atomic.Bool

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	// /snapshot 은 가장 최근 reconcile cycle 의 NoisyNeighbor Top-N 결과를 JSON 으로 노출한다.
	// rca-summarizer 가 webhook handler 에서 본 endpoint 를 호출해 Prometheus query 재계산을
	// 회피하고 30s 응답 임계를 통과한다. 첫 reconcile 전에는 빈 배열을 돌려준다.
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		snap := collector.Snapshot()
		if snap == nil {
			snap = []correlation.NoisyNeighbor{}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(snap); err != nil {
			log.Printf("snapshot encode: %v", err)
		}
	})

	// #100 REST API layer 도입. /api/v1/noisy-neighbor 와 #119 의 /api/v1/cross-node-interference 와
	// #148 의 /api/v1/service-impact 와 #149 의 /api/v1/cross-level 와 #151 의 /api/v1/impact-graph 와
	// swagger UI 부착. handler 는 collector 의 Snapshot / CrossNodeSnapshot / ServiceImpactSnapshot /
	// CrossLevelSnapshot / ImpactGraphSnapshot 을 in-memory read 로만 사용 하므로 scrape hot path 와
	// 분리 되고 추가 부담 없음. collector 가 다섯 인터페이스 를 모두 만족 하므로 동일 인스턴스 를 다섯 번
	// 전달 한다.
	api.NewHandler(collector, collector, collector, collector, collector).Register(mux)
	// #178 synthesis API. Prometheus instant query 로 health / pressure recording rule 을 합성해
	// 헬스 + 압박 위치를 한 응답 (/api/v1/health) 으로 노출한다. range fetch 와 별개 경로라 reconcile
	// hot path 와 무관하다. querier 초기화 실패 시 합성 endpoint 만 비활성되고 기존 API 는 유지된다.
	if iq, err := correlation.NewPrometheusInstantQuerier(cfg.PrometheusURL, cfg.FetchTimeout); err != nil {
		log.Printf("warn: synthesis API disabled, instant querier init failed: %v", err)
	} else {
		// collector 가 SnapshotSource (noisy-neighbor) 를 만족해 /api/v1/events 가 anomaly 와 함께
		// 간섭 사건을 합성한다.
		api.NewSynthesisHandler(iq, collector, collector).Register(mux)
	}
	// #195 진단 신호 추이 API. collector 가 이미 emit 하는 correlation_* 시계열을 range query 로 읽어
	// /api/v1/trends 로 이력을 노출한다. 적재는 collector 가 수행하므로 본 핸들러는 range fetch 만 한다.
	api.NewTrendsHandler(fetcher).Register(mux)
	correlationdocs.SwaggerInfocorrelation.BasePath = "/"
	mux.Handle("/api/v1/swagger/", httpSwagger.Handler(httpSwagger.URL("/api/v1/swagger.json"), httpSwagger.InstanceName("correlation")))
	mux.HandleFunc("/api/v1/swagger.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(correlationdocs.SwaggerInfocorrelation.ReadDoc()))
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("metrics listening on %s", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
		}
	}()

	log.Printf("config: prometheus=%s window=%s step=%s reconcile_interval=%s top_n=%d min_samples=%d lag_steps=%v",
		cfg.PrometheusURL, cfg.Window, cfg.Step, reconcileInterval, topN, cfg.MinSamples, cfg.LagSteps)
	log.Printf("metrics: default=%d extra=%d", len(cfg.DefaultMetrics), len(cfg.ExtraMetrics))

	cycleTimeout := cfg.FetchTimeout * time.Duration(len(cfg.DefaultMetrics)+len(cfg.ExtraMetrics)+1)
	runReconcile := func() {
		reconcileOnce(ctx, corr, collector, health, &ready, topN, cycleTimeout)
	}

	// 첫 reconcile 을 ticker 시작 전 즉시 실행해 첫 reconcile latency 를 reconcileInterval 만큼
	// 지연시키지 않는다.
	runReconcile()

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutdown: %v", ctx.Err())
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(shutdownCtx)
			cancel()
			return
		case <-ticker.C:
			runReconcile()
		}
	}
}

// reconcileOnce 는 reconcile 1 cycle 을 수행하고 결과를 collector / health 에 반영한다. main 의
// 클로저 대신 별도 함수로 분리해 단위 테스트가 mock fetcher 로 본 함수를 직접 호출 가능하게 한다.
func reconcileOnce(
	ctx context.Context,
	corr *correlation.Correlator,
	collector *exporter.Collector,
	health *exporter.Health,
	ready *atomic.Bool,
	topN int,
	cycleTimeout time.Duration,
) {
	cycleStart := time.Now()
	cycleCtx, cancel := context.WithTimeout(ctx, cycleTimeout)
	defer cancel()

	results, err := corr.Correlate(cycleCtx, time.Now())
	if err != nil {
		log.Printf("reconcile error: %v", err)
		health.RecordError()
		return
	}
	neighbors := correlation.SelectTopN(results, topN)
	collector.Replace(neighbors)
	// #84 cross-node interference snapshot 도 동일 reconcile cycle 에서 갱신 한다. CrossNodeEnabled
	// 가 false 면 results 에 IsCrossNode=true 항목 이 없 으므로 빈 슬라이스 가 전달 되어 series 가
	// emit 되지 않는다.
	crossNode := correlation.SelectTopNCrossNode(results, topN)
	collector.ReplaceCrossNode(crossNode)
	// #148 service-impact snapshot 도 동일 reconcile cycle 에서 갱신한다. ServiceImpactEnabled 가
	// false 면 results 에 IsServiceImpact=true 항목이 없으므로 빈 슬라이스가 전달되어 series 가 emit
	// 되지 않는다.
	serviceImpact := correlation.SelectTopNServiceImpact(results, topN)
	collector.ReplaceServiceImpact(serviceImpact)
	// #149 cross-level snapshot 도 동일 reconcile cycle 에서 갱신한다. CrossLevelEnabled 가 false 면
	// results 에 IsCrossLevel=true 항목이 없으므로 빈 슬라이스가 전달되어 series 가 emit 되지 않는다.
	crossLevel := correlation.SelectTopNCrossLevel(results, topN)
	collector.ReplaceCrossLevel(crossLevel)
	duration := time.Since(cycleStart)
	cfg := corr.Config()
	// #151 Phase 1 영향 전파 그래프. neighbors (noisy neighbor Top-N) 를 정점/엣지 그래프로 구성해
	// 갱신한다. ImpactGraphEnabled 가 false 면 빈 그래프를 전달해 node degree series 가 emit 되지 않고
	// API 도 빈 그래프를 돌려 준다. 새 Prometheus fetch 없이 neighbors 만 재사용한다.
	var impactGraph correlation.ImpactGraph
	var impactPaths []correlation.ImpactPath
	pathsTruncated := false
	if cfg.ImpactGraphEnabled {
		impactGraph = correlation.BuildImpactGraph(neighbors)
		// #151 Phase 2 다단계 경로 추출. 동일 그래프에서 근원 suspect 별 transitive 경로를 뽑는다.
		impactPaths, pathsTruncated = correlation.ExtractImpactPaths(impactGraph, cfg.ImpactPathMaxDepth, cfg.ImpactPathMinScore, cfg.ImpactPathMaxPaths)
	}
	collector.ReplaceImpactGraph(impactGraph)
	collector.ReplaceImpactPaths(impactPaths)
	// expectedMetrics 는 활성 layer 가 fetch 하는 distinct query 수다. PlannedQueries 가 layer 간 공유
	// query (node 압박 score) 를 dedup 하므로 RecordCycle 의 observed distinct metric 수와 정합해
	// ReconcilePartial 이 거짓 증가하지 않는다.
	expectedMetrics := len(cfg.PlannedQueries())
	health.RecordCycle(duration, results, neighbors, expectedMetrics)
	ready.Store(true)
	log.Printf("reconcile ok: pairs=%d neighbors=%d cross_node=%d service_impact=%d cross_level=%d graph_nodes=%d graph_edges=%d impact_paths=%d impact_paths_truncated=%v duration=%s", len(results), len(neighbors), len(crossNode), len(serviceImpact), len(crossLevel), len(impactGraph.Nodes), len(impactGraph.Edges), len(impactPaths), pathsTruncated, duration)
}

// hasCLIFlag 는 args 에 -flag, --flag, -flag=, --flag= 패턴이 있는지 검사한다. flag 우선 정책을
// 정확히 구현하기 위해 env 파싱 실패 fallback 시 사용한다.
func hasCLIFlag(args []string, name string) bool {
	single := "-" + name
	double := "--" + name
	for _, arg := range args {
		if arg == single || arg == double ||
			strings.HasPrefix(arg, single+"=") ||
			strings.HasPrefix(arg, double+"=") {
			return true
		}
	}
	return false
}

// applyEnvDuration 은 env 값이 있을 때 dst 를 갱신한다. 빈 값이면 dst 유지. env 가 잘못된 형식일
// 때 동일 의미의 CLI flag 가 제공되어 있으면 env 무시 (flag 가 덮어쓸 예정) 하고 그렇지 않으면
// misconfiguration 을 silent 하게 통과시키지 않도록 fatal 로 격상한다.
func applyEnvDuration(envKey, flagName string, dst *time.Duration) {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		if hasCLIFlag(os.Args[1:], flagName) {
			return
		}
		log.Fatalf("env %s parse: %v", envKey, err)
	}
	*dst = d
}

func applyEnvInt(envKey, flagName string, dst *int) {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		if hasCLIFlag(os.Args[1:], flagName) {
			return
		}
		log.Fatalf("env %s parse: %v", envKey, err)
	}
	*dst = n
}

// applyEnvFloat 은 applyEnvInt 의 float64 판본이다. ImpactPathMinScore 같은 실수 tunable 의 env
// override 에 쓰인다. parse 실패 시 동일 flag 가 제공되면 무시, 아니면 fatal 로 misconfiguration 을
// 조기에 드러낸다.
func applyEnvFloat(envKey, flagName string, dst *float64) {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		if hasCLIFlag(os.Args[1:], flagName) {
			return
		}
		log.Fatalf("env %s parse: %v", envKey, err)
	}
	*dst = f
}
