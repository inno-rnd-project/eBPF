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
	"netobs/internal/selfobs"
)

// lingerAfterStop 은 injector_active=0 으로 reset 한 후 시계열을 유지하는 시간이다. PodMonitor 의
// scrape 주기 (15s) 보다 충분히 길게 두어 마지막 scrape 가 transition 을 정확히 잡도록 한다.
const lingerAfterStop = 30 * time.Second

func main() {
	// main 본체를 runMain 으로 분리한 뒤 return code 를 os.Exit 하나로만 처리해 lease release 등
	// deferred cleanup 이 fail-fast path 에서도 항상 호출되도록 한다. 이전 구조 (log.Fatalf /
	// os.Exit 직접 호출) 는 deferred 실행을 건너뛰어 ConfigMap lease 가 cfg.Duration*2 TTL 만료
	// 까지 남고 다음 injection 이 lease 충돌로 거부되는 silent regression 을 일으켰다.
	os.Exit(runMain())
}

func runMain() int {
	// #102 controller mode 분기. CLI mode 와 controller mode 가 동일 binary 안에서 -mode 플래그로
	// 라우팅된다. 본 분기는 loadConfig 의 flag 파싱 전에 일찍 처리해 CLI 전용 flag (target-pod 필수
	// 검증 등) 의 fatal exit 가 controller mode 진입을 막지 않게 한다.
	// #405 cgroup limit 기반 GOMEMLIMIT. mode 분기 전에 적용해 controller / CLI 양쪽을 커버한다.
	// controller mode 의 go / process collector 는 controller-runtime 기본 registry 가 이미 포함한다.
	selfobs.ApplyMemoryLimit()
	mode := parseModeFlag(os.Args[1:])
	if mode == "controller" {
		return runControllerMode()
	}

	cfg := loadConfig()

	client, err := newK8sClient(cfg.Kubeconfig)
	if err != nil {
		log.Printf("k8s client: %v", err)
		return 1
	}

	reg := prometheus.NewRegistry()
	// #405 프로세스 자기계측. CLI mode 의 자체 registry 에도 표준 collector 를 실어 다른 서비스와
	// 관측 축을 정합시킨다.
	selfobs.RegisterProcessCollectors(reg)
	collector := exporter.NewCollector()
	reg.MustRegister(collector)
	health := exporter.NewHealth(reg)

	kind := loadgen.Kind(cfg.Kind)

	// 안전 가드 4 종은 lease acquire 전에 통과시킨다. lease 획득 전 실패는 release 가 필요 없다.
	if err := safety.CheckDuration(cfg.Duration); err != nil {
		health.RecordRun(kind, "skipped_gate")
		log.Printf("safety: %v", err)
		return 1
	}
	if err := safety.CheckIntensity(kind, cfg.Intensity); err != nil {
		health.RecordRun(kind, "skipped_gate")
		log.Printf("safety: %v", err)
		return 1
	}
	if err := safety.CheckClusterLabel(context.Background(), client, cfg.AllowClusterLabel); err != nil {
		health.RecordRun(kind, "skipped_gate")
		log.Printf("safety: %v", err)
		return 1
	}
	if err := verifyTargetPod(context.Background(), client, cfg.TargetNamespace, cfg.TargetPod); err != nil {
		health.RecordRun(kind, "skipped_gate")
		log.Printf("target pod: %v", err)
		return 1
	}

	// 동일 target 동시 injection 차단. lease TTL 은 duration 의 2 배로 두어 cleanup 실패 시에도
	// 일정 시간 뒤 자동 해제되도록 한다.
	release, err := safety.AcquireLock(context.Background(), client, cfg.LockNamespace,
		cfg.TargetNamespace, cfg.TargetPod, cfg.LockHolder, cfg.Duration*2)
	if err != nil {
		health.RecordRun(kind, "skipped_gate")
		log.Printf("acquire lock: %v", err)
		return 1
	}
	defer release()

	gen, err := loadgen.New(kind, client)
	if err != nil {
		log.Printf("loadgen: %v", err)
		return 1
	}

	var ready atomic.Bool
	srv := startHTTPServer(cfg.ListenAddr, reg, &ready)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	// readyz 를 baseline fetch 전에 200 으로 전환해 PodMonitor 의 첫 scrape 가 endpoint not ready
	// 로 skip 되지 않도록 한다.
	ready.Store(true)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runInjection(ctx, cfg, client, gen, collector, health, &ready); err != nil {
		log.Printf("injection failed: %v", err)
		health.RecordRun(kind, "error")
		return 1
	}
	health.RecordRun(kind, "ok")
	return 0
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
	// -mode 는 parseModeFlag 가 이미 처리 했지만 flag.Parse 가 unknown flag 로 reject 하지 않도록
	// 본 flagset 에 dummy 로 등록 한다. 값은 본 함수 흐름 에 사용 되지 않는다.
	fs.String("mode", "cli", "execution mode (cli|controller); routing handled in main")
	fs.StringVar(&c.Kind, "kind", c.Kind, "load kind: cpu | memory | network | gpu")
	fs.StringVar(&c.TargetNamespace, "target-namespace", c.TargetNamespace, "target Pod namespace (env TARGET_NAMESPACE)")
	fs.StringVar(&c.TargetPod, "target-pod", c.TargetPod, "target Pod name")
	fs.StringVar(&c.TargetNode, "target-node", c.TargetNode, "target node name (defaults to target Pod's node)")
	fs.StringVar(&c.SpawnNamespace, "spawn-namespace", c.SpawnNamespace, "namespace where stress Pods are created")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "load duration (max 30m)")
	fs.DurationVar(&c.BaselineWindow, "baseline-window", c.BaselineWindow, "baseline / impact measurement window")
	fs.StringVar(&c.Intensity, "intensity", c.Intensity, "load intensity (cpu millis e.g. 500m, memory K8s Quantity e.g. 512Mi or 1Gi, network bandwidth e.g. 100M, gpu utilization percent 1-100 e.g. 80)")
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
	case loadgen.KindCPU, loadgen.KindMemory, loadgen.KindNetwork, loadgen.KindGPU:
	default:
		log.Fatalf("unknown kind %q (expected cpu | memory | network | gpu)", c.Kind)
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
	case loadgen.KindMemory:
		return "512Mi"
	case loadgen.KindNetwork:
		return "100M"
	case loadgen.KindGPU:
		return "80"
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
	// rate window 1m 은 step 30s 의 약 2 배로 짧다. window 가 길면 baseline 측 sample 이 impact
	// 평균에 섞이는 dilution (DURATION 5 분일 때 rate window 5 분이면 impact 평균의 절반이 baseline
	// 으로 희석) 이 발생해 score 가 부풀려지지 않고 줄어든다. 1m 은 sample variance 가 다소 크지만
	// 본 도구의 5 ~ 30 분 부하 윈도우에서 baseline 격리 정확도가 더 우선이다.
	query := `histogram_quantile(0.99, sum by(node, src_namespace, src_pod, src_pod_uid, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[1m])))`
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
