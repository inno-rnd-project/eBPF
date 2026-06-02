package main

import (
	"flag"
	"log"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	injectorv1alpha1 "netobs/api/v1alpha1"
	injectorcontroller "netobs/internal/injector/controller"
)

// controllerScheme 은 manager 에 등록되는 scheme 이다. core API 와 #102 의 LoadScenario CRD 가
// 포함된다.
var controllerScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(controllerScheme))
	utilruntime.Must(injectorv1alpha1.AddToScheme(controllerScheme))
}

// controllerConfig 는 controller mode 의 runtime 입력이다. CLI mode 의 config 와 일부 환경 변수를
// 공유 하지만 controller 전용 필드 (LeaderElection / Metrics port / WatchNamespace) 가 추가된다.
type controllerConfig struct {
	MetricsAddr            string
	HealthAddr             string
	LeaderElection         bool
	LeaderElectionID       string
	LeaderElectionNS       string
	WatchNamespace         string
	PrometheusURL          string
	AllowClusterLabel      string
	SpikeAssertTimeout     time.Duration
	SpikeAssertPollEvery   time.Duration
	ReconcileBackoffWindow time.Duration
}

// loadControllerConfig 는 controller mode 전용 flag/env 를 파싱한다. CLI mode 의 loadConfig 와
// 분리 해 controller 전용 옵션 만 정의 한다.
func loadControllerConfig() *controllerConfig {
	c := &controllerConfig{
		MetricsAddr:            envOr("CONTROLLER_METRICS_ADDR", ":9841"),
		HealthAddr:             envOr("CONTROLLER_HEALTH_ADDR", ":9842"),
		LeaderElection:         envOr("CONTROLLER_LEADER_ELECTION", "true") == "true",
		LeaderElectionID:       envOr("CONTROLLER_LEADER_ELECTION_ID", "loadscenario.injector.netobs.io"),
		LeaderElectionNS:       envOr("CONTROLLER_LEADER_ELECTION_NAMESPACE", "ebpf-project"),
		WatchNamespace:         envOr("CONTROLLER_WATCH_NAMESPACE", ""),
		PrometheusURL:          envOr("PROMETHEUS_URL", "http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090"),
		AllowClusterLabel:      envOr("INJECTOR_ALLOW_CLUSTER_LABEL", "environment=dev"),
		SpikeAssertTimeout:     envDuration("SPIKE_ASSERT_TIMEOUT", 5*time.Minute),
		SpikeAssertPollEvery:   envDuration("SPIKE_ASSERT_POLL_EVERY", 30*time.Second),
		ReconcileBackoffWindow: envDuration("RECONCILE_BACKOFF", 30*time.Second),
	}

	fs := flag.NewFlagSet("workload-injector-controller", flag.ContinueOnError)
	// 본 flagset 은 controller mode 전용 flag 만 선언 한다. CLI 전용 flag (-target-pod 등) 와의
	// duplicate 정의 충돌을 회피 한다. -mode 자체 는 parseModeFlag 가 이미 처리해 본 fs 에는 등록
	// 하지 않으며 unknown flag 무시 를 위해 ContinueOnError 만 사용 한다.
	fs.String("mode", "controller", "execution mode (cli|controller); parsed earlier in main")
	fs.StringVar(&c.MetricsAddr, "controller-metrics-addr", c.MetricsAddr, "controller metrics endpoint listen address")
	fs.StringVar(&c.HealthAddr, "controller-health-addr", c.HealthAddr, "controller healthz/readyz listen address")
	fs.BoolVar(&c.LeaderElection, "controller-leader-election", c.LeaderElection, "enable leader election (single active controller)")
	fs.StringVar(&c.LeaderElectionID, "controller-leader-election-id", c.LeaderElectionID, "leader election lease name")
	fs.StringVar(&c.LeaderElectionNS, "controller-leader-election-namespace", c.LeaderElectionNS, "leader election lease namespace")
	fs.StringVar(&c.WatchNamespace, "controller-watch-namespace", c.WatchNamespace, "namespace to watch for LoadScenario (empty = all namespaces)")
	fs.StringVar(&c.PrometheusURL, "prometheus-url", c.PrometheusURL, "Prometheus base URL (spike alert assertion)")
	fs.StringVar(&c.AllowClusterLabel, "allow-cluster-label", c.AllowClusterLabel, "required node label for cluster safety gate")
	fs.DurationVar(&c.SpikeAssertTimeout, "spike-assert-timeout", c.SpikeAssertTimeout, "spike alert polling window after run end")
	fs.DurationVar(&c.SpikeAssertPollEvery, "spike-assert-poll-every", c.SpikeAssertPollEvery, "spike alert polling interval")
	fs.DurationVar(&c.ReconcileBackoffWindow, "reconcile-backoff", c.ReconcileBackoffWindow, "default reconcile re-queue backoff")

	if err := fs.Parse(os.Args[1:]); err != nil && err != flag.ErrHelp {
		log.Printf("controller flag parse: %v", err)
	}
	return c
}

// runControllerMode 는 -mode=controller 진입점이다. controller-runtime manager 를 부트스트랩 하고
// LoadScenario reconciler 등록 placeholder 를 두며 signal 수신 시 graceful shutdown 한다. 실제
// reconciler 구현 은 후속 commit (c4) 에서 본 placeholder 자리 에 채워진다.
func runControllerMode() int {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	cfg := loadControllerConfig()

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		log.Printf("controller: get rest config: %v", err)
		return 1
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  controllerScheme,
		Metrics:                 metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress:  cfg.HealthAddr,
		LeaderElection:          cfg.LeaderElection,
		LeaderElectionID:        cfg.LeaderElectionID,
		LeaderElectionNamespace: cfg.LeaderElectionNS,
	})
	if err != nil {
		log.Printf("controller: manager bootstrap: %v", err)
		return 1
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Printf("controller: add healthz: %v", err)
		return 1
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Printf("controller: add readyz: %v", err)
		return 1
	}

	// LoadScenario reconciler 등록. K8sClient 는 internal/injector/safety 와 internal/injector/loadgen
	// 이 kubernetes.Interface 를 직접 요구 하므로 controller-runtime client 와 별도로 client-go
	// kubernetes.Clientset 을 manager rest config 로 부터 생성 해 reconciler 에 주입 한다.
	k8sClient, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		log.Printf("controller: kubernetes clientset: %v", err)
		return 1
	}
	holder, _ := os.Hostname()
	if holder == "" {
		holder = "workload-injector-controller"
	}
	reconciler := &injectorcontroller.LoadScenarioReconciler{
		K8sClient:         k8sClient,
		AllowClusterLabel: cfg.AllowClusterLabel,
		LockNamespace:     cfg.LeaderElectionNS,
		LockHolder:        holder,
		// SpikeAsserter 는 c5 commit 에서 prometheus query 구현체 가 주입 된다. 본 commit 에서는
		// nil 로 두어 spec.spikeAlertAssertion 이 true 더라도 status 갱신 만 skip 한다.
		SpikeAsserter: nil,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		log.Printf("controller: reconciler SetupWithManager: %v", err)
		return 1
	}

	log.Printf("controller: starting manager (leader_election=%t metrics=%s)", cfg.LeaderElection, cfg.MetricsAddr)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Printf("controller: manager exit: %v", err)
		return 1
	}
	return 0
}
