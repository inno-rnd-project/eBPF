package config

import (
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

	flag.StringVar(&cfg.TargetIP, "target-ip", cfg.TargetIP, "destination Pod IPv4 to trace; empty means observe all")
	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address for /metrics, /healthz, /readyz")
	flag.BoolVar(&cfg.PrintEvents, "print-events", cfg.PrintEvents, "print events to stdout")
	flag.BoolVar(&cfg.PodMetricsEnabled, "pod-metrics", cfg.PodMetricsEnabled, "emit per-pod-instance metrics (netobs_pod_stage_*); disable on large clusters to cap Prometheus cardinality")
	flag.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "observed Kubernetes node name")
	flag.DurationVar(&cfg.MetadataRefresh, "metadata-refresh", cfg.MetadataRefresh, "Kubernetes metadata refresh interval")
	flag.StringVar(&cfg.DropReasonFormatPath, "drop-reason-format", cfg.DropReasonFormatPath, "skb:kfree_skb tracepoint format path")
	flag.Parse()

	if cfg.TargetIP != "" && net.ParseIP(cfg.TargetIP).To4() == nil {
		return Config{}, fmt.Errorf("invalid -target-ip: %s", cfg.TargetIP)
	}

	if cfg.MetadataRefresh <= 0 {
		return Config{}, fmt.Errorf("invalid -metadata-refresh: must be > 0")
	}

	return cfg, nil
}
