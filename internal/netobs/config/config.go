package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

type Config struct {
	TargetIP             string
	ListenAddr           string
	PrintEvents          bool
	PodMetricsEnabled    bool
	NodeName             string
	MetadataRefresh      time.Duration
	DropReasonFormatPath string
}

func getenv(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func getenvBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "y"
}

func getenvDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s: %q", key, v)
	}
	return d, nil
}

func hostnameOr(fallback string) string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return fallback
	}
	return h
}

func Parse() (Config, error) {
	metadataRefresh, err := getenvDuration("KUBE_METADATA_REFRESH", 30*time.Second)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		TargetIP:             getenv("TARGET_IP", ""),
		ListenAddr:           getenv("LISTEN_ADDR", ":9810"),
		PrintEvents:          getenvBool("PRINT_EVENTS", false),
		PodMetricsEnabled:    getenvBool("POD_METRICS_ENABLED", true),
		NodeName:             getenv("NODE_NAME", hostnameOr("unknown-node")),
		MetadataRefresh:      metadataRefresh,
		DropReasonFormatPath: getenv("DROP_REASON_FORMAT_PATH", "/sys/kernel/tracing/events/skb/kfree_skb/format"),
	}

	fs := flag.NewFlagSet("netobs-agent", flag.ContinueOnError)
	fs.StringVar(&cfg.TargetIP, "target-ip", cfg.TargetIP, "destination Pod IPv4 to trace; empty means observe all")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address for /metrics, /healthz, /readyz")
	fs.BoolVar(&cfg.PrintEvents, "print-events", cfg.PrintEvents, "print events to stdout")
	fs.BoolVar(&cfg.PodMetricsEnabled, "pod-metrics", cfg.PodMetricsEnabled, "emit per-pod-instance metrics (netobs_pod_stage_*); disable on large clusters to cap Prometheus cardinality")
	fs.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "observed Kubernetes node name")
	fs.DurationVar(&cfg.MetadataRefresh, "metadata-refresh", cfg.MetadataRefresh, "Kubernetes metadata refresh interval")
	fs.StringVar(&cfg.DropReasonFormatPath, "drop-reason-format", cfg.DropReasonFormatPath, "skb:kfree_skb tracepoint format path")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// -h/-help 요청은 flag 패키지가 usage를 출력한 뒤 ErrHelp를 반환한다.
		// 사용자 의도된 정상 경로이므로 exit 0으로 종료한다.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return Config{}, err
	}

	if cfg.TargetIP != "" && net.ParseIP(cfg.TargetIP).To4() == nil {
		return Config{}, fmt.Errorf("invalid -target-ip: %s", cfg.TargetIP)
	}

	if cfg.MetadataRefresh <= 0 {
		return Config{}, fmt.Errorf("invalid -metadata-refresh: must be > 0")
	}

	return cfg, nil
}
