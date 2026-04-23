// Package config는 gpuobs 에이전트의 실행 시 설정을 정의하며, env 값이 기본값이 되고
// CLI flag가 지정되면 env를 덮어쓰는 순서를 따른다. env 형식 오류는 warn 로그를 남기고
// 기본값으로 폴백해 flag가 여전히 최종 값을 덮어쓸 수 있게 한다.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log"
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
// env 값이 기본값이 되고 CLI flag가 지정되면 env를 덮어쓴다. env가 형식 오류(예:
// `GPU_POLL_INTERVAL`의 duration 파싱 실패)인 경우 warn 로그를 남기고 기본값으로 폴백해
// -poll-interval 등 flag가 명시되면 그 값이 최종적으로 이기도록 한다. ConfigMap/DaemonSet
// 오타는 warn 로그로 계속 드러나 완전히 숨겨지지 않는다.
// NodeName이 비어 있으면 os.Hostname 결과로 채워진다.
func Parse() (Config, error) {
	pollInterval, err := getenvDuration("GPU_POLL_INTERVAL", 5*time.Second)
	if err != nil {
		// env 형식 오류는 "env < flag 우선순위" 를 끊지 않도록 warn 후 기본값으로 폴백한다.
		// 이후 -poll-interval flag가 명시되면 그 값이 최종적으로 덮어쓴다.
		log.Printf("warn: %v; using default %v", err, pollInterval)
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

// getenvDuration은 key env를 duration으로 파싱해 반환한다.
// 형식 오류일 때는 호출자가 "env < flag 우선순위" 약속을 유지할 수 있도록 기본값과
// 함께 에러를 돌려주어, 호출자가 warn 로그 후 폴백을 선택할 수 있게 한다.
func getenvDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("invalid duration for %s: %q", key, v)
	}
	return d, nil
}
