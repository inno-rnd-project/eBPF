// env가 기본값이 되고 CLI flag가 지정되면 그 값이 env를 덮어쓰는 순서를 따른다.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Config는 gpuobs 에이전트의 실행 시 설정을 담는다.
type Config struct {
	ListenAddr        string
	NodeName          string
	GPUPollInterval   time.Duration
	GPUMetricsEnabled bool
}

// Parse는 env와 CLI flag를 읽어 Config를 구성해 반환한다.
// env가 기본값으로 들어가고 flag가 지정되면 그 값이 최종 값이 된다.
// NodeName이 비어 있으면 os.Hostname 결과로 채워진다.
func Parse() (Config, error) {
	pollInterval, err := getenvDuration("GPU_POLL_INTERVAL", 5*time.Second)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		ListenAddr:        getenvDefault("LISTEN_ADDR", ":9820"),
		NodeName:          getenvDefault("NODE_NAME", ""),
		GPUPollInterval:   pollInterval,
		GPUMetricsEnabled: getenvBool("GPU_METRICS_ENABLED", true),
	}

	fs := flag.NewFlagSet("gpuobs-agent", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address for metrics and health endpoints")
	fs.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "observed Kubernetes node name (defaults to hostname when empty)")
	fs.DurationVar(&cfg.GPUPollInterval, "poll-interval", cfg.GPUPollInterval, "NVML polling interval for device snapshots")
	fs.BoolVar(&cfg.GPUMetricsEnabled, "gpu-metrics", cfg.GPUMetricsEnabled, "emit per-device gpuobs_device_* metrics; disable to suppress device-level collection")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// -h/-help 요청은 flag 패키지가 usage를 출력한 뒤 ErrHelp를 반환한다.
		// 사용자 의도된 정상 경로이므로 exit 0으로 종료한다.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return Config{}, err
	}

	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return Config{}, fmt.Errorf("listen address must not be empty")
	}

	if cfg.GPUPollInterval <= 0 {
		return Config{}, fmt.Errorf("invalid -poll-interval: must be > 0")
	}

	if strings.TrimSpace(cfg.NodeName) == "" {
		host, err := os.Hostname()
		if err != nil {
			return Config{}, fmt.Errorf("node name empty and hostname unavailable: %w", err)
		}
		cfg.NodeName = host
	}

	return cfg, nil
}

func getenvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
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
