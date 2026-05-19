// workload-injector 는 운영자가 dev 또는 staging 환경에서 특정 노드 / Pod 에 합성 부하를 트리거해
// correlation 분석 layer 의 산출을 검증 가능하게 하는 Job 형태 도구다. 부하 시작 / 종료 시점을
// injector_active 메트릭으로 마킹하고 부하 윈도우 전후의 victim latency 비교를 correlation_blast_radius_score
// 로 노출한다.
package main

import (
	"context"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"netobs/internal/correlation"
	"netobs/internal/injector/blastradius"
	"netobs/internal/injector/exporter"
	"netobs/internal/injector/loadgen"
	"netobs/internal/injector/safety"
)

// lingerAfterStop 은 injector_active=0 으로 reset 한 후 시계열을 유지하는 시간이다. PodMonitor 의
// scrape 주기 (15s) 보다 충분히 길게 두어 마지막 scrape 가 transition 을 정확히 잡도록 한다.
const lingerAfterStop = 30 * time.Second

func main() {
	cfg := loadConfig()

	client, err := newK8sClient(cfg.Kubeconfig)
	if err != nil {
		log.Fatalf("k8s client: %v", err)
	}

	reg := prometheus.NewRegistry()
	collector := exporter.NewCollector()
	reg.MustRegister(collector)
	health := exporter.NewHealth(reg)

	// 안전 가드 3 종을 본격 실행 전에 모두 통과시킨다. silent misuse 를 막기 위해 실패는 fatal 로
	// 격상하고 skipped_gate status 로 self-health 에 기록.
	if err := safety.CheckDuration(cfg.Duration); err != nil {
		health.RecordRun(loadgen.Kind(cfg.Kind), "skipped_gate")
		log.Fatalf("safety: %v", err)
	}
	if err := safety.CheckIntensity(loadgen.Kind(cfg.Kind), cfg.Intensity); err != nil {
		health.RecordRun(loadgen.Kind(cfg.Kind), "skipped_gate")
		log.Fatalf("safety: %v", err)
	}
	if err := safety.CheckClusterLabel(context.Background(), client, cfg.AllowClusterLabel); err != nil {
		health.RecordRun(loadgen.Kind(cfg.Kind), "skipped_gate")
		log.Fatalf("safety: %v", err)
	}

	if err := verifyTargetPod(context.Background(), client, cfg.TargetNamespace, cfg.TargetPod); err != nil {
		health.RecordRun(loadgen.Kind(cfg.Kind), "skipped_gate")
		log.Fatalf("target pod: %v", err)
	}

	// 동일 target 동시 injection 차단. lease TTL 은 duration 의 2 배로 두어 cleanup 실패 시에도
	// 일정 시간 뒤 자동 해제되도록 한다.
	release, err := safety.AcquireLock(context.Background(), client, cfg.LockNamespace,
		cfg.TargetNamespace, cfg.TargetPod, cfg.LockHolder, cfg.Duration*2)
	if err != nil {
		health.RecordRun(loadgen.Kind(cfg.Kind), "skipped_gate")
		log.Fatalf("acquire lock: %v", err)
	}
	defer release()

	gen, err := loadgen.New(loadgen.Kind(cfg.Kind), client)
	if err != nil {
		log.Fatalf("loadgen: %v", err)
	}

	var ready atomic.Bool
	srv := startHTTPServer(cfg.ListenAddr, reg, &ready)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if err := runInjection(ctx, cfg, client, gen, collector, health, &ready); err != nil {
		log.Printf("injection failed: %v", err)
		health.RecordRun(loadgen.Kind(cfg.Kind), "error")
		os.Exit(1)
	}
	health.RecordRun(loadgen.Kind(cfg.Kind), "ok")
}

// config 는 운영자 입력을 모은 구조체다. env 와 flag 둘 다 지원하며 flag 가 우선이다.
type config struct {
	Kind              string
	TargetNamespace   string
	TargetPod         string
	TargetNode        string
	SpawnNamespace    string
	Duration          time.Duration
	BaselineWindow    time.Duration
	Intensity         string
	PrometheusURL     string
	FetchTimeout      time.Duration
	MaxVictims        int
	ListenAddr        string
	Kubeconfig        string
	AllowClusterLabel string
	LockNamespace     string
	LockHolder        string
}

func loadConfig() *config {
	c := &config{
		Kind:              envOr("KIND", "cpu"),
		TargetNamespace:   envOr("TARGET_NAMESPACE", ""),
		TargetPod:         envOr("TARGET_POD", ""),
		TargetNode:        envOr("TARGET_NODE", ""),
		SpawnNamespace:    envOr("SPAWN_NAMESPACE", "ebpf-project"),
		Intensity:         envOr("INTENSITY", ""),
		PrometheusURL:     envOr("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090"),
		ListenAddr:        envOr("LISTEN_ADDR", ":9840"),
		Kubeconfig:        envOr("KUBECONFIG", ""),
		AllowClusterLabel: envOr("INJECTOR_ALLOW_CLUSTER_LABEL", "environment=dev"),
		LockNamespace:     envOr("LOCK_NAMESPACE", "ebpf-project"),
		LockHolder:        envOr("HOSTNAME", "workload-injector"),
	}
	c.Duration = envDuration("DURATION", 5*time.Minute)
	c.BaselineWindow = envDuration("BASELINE_WINDOW", 10*time.Minute)
	c.FetchTimeout = envDuration("FETCH_TIMEOUT", 30*time.Second)
	c.MaxVictims = envInt("MAX_VICTIMS", 20)

	fs := flag.NewFlagSet("workload-injector", flag.ContinueOnError)
	fs.StringVar(&c.Kind, "kind", c.Kind, "load kind: cpu | network | gpu")
	fs.StringVar(&c.TargetNamespace, "target-namespace", c.TargetNamespace, "target Pod namespace (env TARGET_NAMESPACE)")
	fs.StringVar(&c.TargetPod, "target-pod", c.TargetPod, "target Pod name")
	fs.StringVar(&c.TargetNode, "target-node", c.TargetNode, "target node name (defaults to target Pod's node)")
	fs.StringVar(&c.SpawnNamespace, "spawn-namespace", c.SpawnNamespace, "namespace where stress Pods are created")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "load duration (max 30m)")
	fs.DurationVar(&c.BaselineWindow, "baseline-window", c.BaselineWindow, "baseline / impact measurement window")
	fs.StringVar(&c.Intensity, "intensity", c.Intensity, "load intensity (cpu millis e.g. 500m, network bandwidth e.g. 100M, gpu count e.g. 1)")
	fs.StringVar(&c.PrometheusURL, "prometheus-url", c.PrometheusURL, "Prometheus base URL")
	fs.DurationVar(&c.FetchTimeout, "fetch-timeout", c.FetchTimeout, "HTTP timeout for Prometheus query_range")
	fs.IntVar(&c.MaxVictims, "max-victims", c.MaxVictims, "maximum victim Pods to compute blast radius for")
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "metrics server listen address")
	fs.StringVar(&c.Kubeconfig, "kubeconfig", c.Kubeconfig, "path to kubeconfig (empty uses in-cluster config)")
	fs.StringVar(&c.AllowClusterLabel, "allow-cluster-label", c.AllowClusterLabel, "required node label (key=value) for cluster safety gate (empty disables)")
	fs.StringVar(&c.LockNamespace, "lock-namespace", c.LockNamespace, "namespace where ConfigMap lease for concurrent injection gating lives")
	fs.StringVar(&c.LockHolder, "lock-holder", c.LockHolder, "lease holder identifier (defaults to HOSTNAME)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatalf("flag parse: %v", err)
	}

	if c.TargetPod == "" || c.TargetNamespace == "" {
		log.Fatalf("target-namespace and target-pod are required")
	}
	if c.Duration <= 0 {
		log.Fatalf("duration must be positive")
	}
	if c.Duration > 30*time.Minute {
		log.Fatalf("duration must be <= 30m (safety guard)")
	}
	if c.BaselineWindow <= 0 {
		log.Fatalf("baseline-window must be positive")
	}
	if c.MaxVictims <= 0 {
		log.Fatalf("max-victims must be positive")
	}
	switch loadgen.Kind(c.Kind) {
	case loadgen.KindCPU, loadgen.KindNetwork, loadgen.KindGPU:
	default:
		log.Fatalf("unknown kind %q (expected cpu | network | gpu)", c.Kind)
	}
	if c.Intensity == "" {
		c.Intensity = defaultIntensity(loadgen.Kind(c.Kind))
	}
	return c
}

func defaultIntensity(kind loadgen.Kind) string {
	switch kind {
	case loadgen.KindCPU:
		return "500m"
	case loadgen.KindNetwork:
		return "100M"
	case loadgen.KindGPU:
		return "1"
	}
	return ""
}

func newK8sClient(kubeconfig string) (kubernetes.Interface, error) {
	if kubeconfig != "" {
		c, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		return kubernetes.NewForConfig(c)
	}
	c, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(c)
}

func verifyTargetPod(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("not found: %s/%s", namespace, name)
		}
		return err
	}
	if pod.Spec.NodeName == "" {
		return fmt.Errorf("not scheduled yet: %s/%s", namespace, name)
	}
	return nil
}

func startHTTPServer(addr string, reg *prometheus.Registry, ready *atomic.Bool) *http.Server {
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
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("metrics listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
		}
	}()
	return srv
}

// runInjection 은 injection cycle 1 회를 수행한다. 각 단계에서 발생한 에러는 health.RecordError 로
// stage 별 분류하고 cleanup 흐름이 보장되도록 defer 로 Stop 호출을 등록한다.
func runInjection(
	ctx context.Context,
	cfg *config,
	client kubernetes.Interface,
	gen loadgen.LoadGenerator,
	collector *exporter.Collector,
	health *exporter.Health,
	ready *atomic.Bool,
) error {
	// target node 가 비어 있으면 target Pod 의 nodeName 으로 자동 채움.
	if cfg.TargetNode == "" {
		pod, err := client.CoreV1().Pods(cfg.TargetNamespace).Get(ctx, cfg.TargetPod, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("resolve target node: %w", err)
		}
		cfg.TargetNode = pod.Spec.NodeName
	}

	log.Printf("config: kind=%s target=%s/%s@%s duration=%s baseline=%s intensity=%s",
		cfg.Kind, cfg.TargetNamespace, cfg.TargetPod, cfg.TargetNode, cfg.Duration, cfg.BaselineWindow, cfg.Intensity)

	fetcher, err := correlation.NewPrometheusFetcher(cfg.PrometheusURL, cfg.FetchTimeout)
	if err != nil {
		health.RecordError(loadgen.Kind(cfg.Kind), "baseline_fetch")
		return fmt.Errorf("fetcher init: %w", err)
	}

	// 1. baseline fetch.
	baselineEnd := time.Now()
	baselineStart := baselineEnd.Add(-cfg.BaselineWindow)
	baseline, err := fetchVictimLatency(ctx, fetcher, cfg, baselineStart, baselineEnd)
	if err != nil {
		health.RecordError(loadgen.Kind(cfg.Kind), "baseline_fetch")
		return fmt.Errorf("baseline fetch: %w", err)
	}
	log.Printf("baseline: %d victim candidates", len(baseline))
	ready.Store(true)

	// 2. 부하 시작 + active=1.
	collector.SetActive(cfg.TargetNamespace, cfg.TargetPod, cfg.TargetNode, loadgen.Kind(cfg.Kind), 1)
	loadStart := time.Now()
	startErr := gen.Start(ctx, loadgen.Params{
		TargetNode:      cfg.TargetNode,
		TargetNamespace: cfg.TargetNamespace,
		TargetPod:       cfg.TargetPod,
		SpawnNamespace:  cfg.SpawnNamespace,
		Duration:        cfg.Duration,
		Intensity:       cfg.Intensity,
	})
	// defer 로 Stop 호출 등록. start 가 부분 spawn 후 실패해도 cleanup.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := gen.Stop(stopCtx); err != nil {
			log.Printf("loadgen stop: %v", err)
			health.RecordError(loadgen.Kind(cfg.Kind), "loadgen_stop")
		}
	}()
	if startErr != nil {
		collector.SetActive(cfg.TargetNamespace, cfg.TargetPod, cfg.TargetNode, loadgen.Kind(cfg.Kind), 0)
		health.RecordError(loadgen.Kind(cfg.Kind), "loadgen_start")
		return fmt.Errorf("loadgen start: %w", startErr)
	}

	// 3. Duration 만큼 대기. ctx cancel 시 즉시 종료해 cleanup 흐름으로.
	select {
	case <-ctx.Done():
		log.Printf("interrupted before duration end: %v", ctx.Err())
	case <-time.After(cfg.Duration):
	}

	// 4. active=0 으로 transition.
	collector.SetActive(cfg.TargetNamespace, cfg.TargetPod, cfg.TargetNode, loadgen.Kind(cfg.Kind), 0)
	loadEnd := time.Now()
	health.RecordDuration(loadgen.Kind(cfg.Kind), loadEnd.Sub(loadStart))

	// 5. impact fetch. 부하 윈도우 (loadStart ~ loadEnd) 와 정확히 일치하는 구간만 fetch 해
	// baseline 측 데이터가 impact 평균에 섞이지 않게 한다. step 30s 기준 5 분 부하면 sample 10 개
	// 이상이라 평균 산출 안정성에 충분하다.
	impact, err := fetchVictimLatency(ctx, fetcher, cfg, loadStart, loadEnd)
	if err != nil {
		health.RecordError(loadgen.Kind(cfg.Kind), "impact_fetch")
		return fmt.Errorf("impact fetch: %w", err)
	}

	// 6. blast radius 산출 후 snapshot 교체.
	results := computeBlastResults(cfg, baseline, impact)
	collector.ReplaceBlast(results)
	log.Printf("blast results: %d victims, %d ok", len(results), countOK(results))

	// 7. linger 동안 메트릭 유지 후 ClearActive.
	select {
	case <-ctx.Done():
	case <-time.After(lingerAfterStop):
	}
	collector.ClearActive()
	return nil
}

// fetchVictimLatency 는 victim 후보별 latency p99 시계열을 fetch 해 (label set, []float64) 맵으로
// 반환한다. correlation 패키지의 Fetcher 를 reuse 한다.
func fetchVictimLatency(
	ctx context.Context,
	fetcher correlation.Fetcher,
	cfg *config,
	start, end time.Time,
) (map[blastradius.VictimCandidate][]float64, error) {
	query := `histogram_quantile(0.99, sum by(node, src_namespace, src_pod, src_pod_uid, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))`
	series, err := fetcher.Fetch(ctx, query, start, end, 30*time.Second)
	if err != nil {
		return nil, err
	}
	all := make([]blastradius.VictimCandidate, 0, len(series))
	samples := make(map[blastradius.VictimCandidate][]float64, len(series))
	for _, s := range series {
		c := blastradius.VictimCandidate{
			Namespace: s.Series.Labels["src_namespace"],
			Pod:       s.Series.Labels["src_pod"],
			PodUID:    s.Series.Labels["src_pod_uid"],
			Node:      s.Series.Labels["node"],
		}
		if c.Pod == "" {
			continue
		}
		all = append(all, c)
		values := make([]float64, 0, len(s.Series.Samples))
		for _, p := range s.Series.Samples {
			values = append(values, p.Value)
		}
		samples[c] = values
	}
	candidates := blastradius.VictimCandidates(all, cfg.TargetNode, cfg.TargetNamespace, cfg.TargetPod, cfg.MaxVictims)
	filtered := make(map[blastradius.VictimCandidate][]float64, len(candidates))
	for _, c := range candidates {
		if v, ok := samples[c]; ok {
			filtered[c] = v
		}
	}
	return filtered, nil
}

func computeBlastResults(
	cfg *config,
	baseline, impact map[blastradius.VictimCandidate][]float64,
) []exporter.BlastResult {
	out := make([]exporter.BlastResult, 0, len(baseline))
	for victim, baseSamples := range baseline {
		impactSamples := impact[victim]
		score, status := blastradius.Compute(baseSamples, impactSamples)
		baseMean := meanOf(baseSamples)
		impactMean := meanOf(impactSamples)
		out = append(out, exporter.BlastResult{
			TargetNamespace: cfg.TargetNamespace,
			TargetPod:       cfg.TargetPod,
			TargetNode:      cfg.TargetNode,
			Kind:            loadgen.Kind(cfg.Kind),
			Victim:          victim,
			Score:           score,
			Status:          status,
			Baseline:        baseMean,
			Impact:          impactMean,
		})
	}
	return out
}

// meanOf 는 NaN / Inf 만 제거하고 0 을 포함한 finite 값의 평균을 산출한다. latency 시계열에서 0 도
// 의미 있는 값 (트래픽 없음) 일 수 있어 평균 계산에서 0 을 임의로 제외하면 baseline 측 데이터가
// 정상값으로 오인되어 impact 비교가 왜곡될 수 있다.
func meanOf(in []float64) float64 {
	if len(in) == 0 {
		return 0
	}
	var sum float64
	var n int
	for _, v := range in {
		if v != v || v > 1e308 || v < -1e308 {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func countOK(results []exporter.BlastResult) int {
	n := 0
	for _, r := range results {
		if r.Status == blastradius.StatusOK {
			n++
		}
	}
	return n
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("env %s parse: %v", key, err)
	}
	return d
}

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Fatalf("env %s parse: %v", key, err)
	}
	return n
}
