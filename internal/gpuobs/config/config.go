package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// Config는 gpuobs 에이전트의 실행 시 설정을 담는다.
type Config struct {
	ListenAddr string
	NodeName   string
}

// Parse는 env와 CLI flag를 읽어 Config를 구성해 반환한다.
// env가 기본값으로 들어가고 flag가 지정되면 그 값이 최종 값이 된다.
// NodeName이 비어 있으면 os.Hostname 결과로 채워진다.
func Parse() (Config, error) {
	cfg := Config{
		ListenAddr: getenvDefault("LISTEN_ADDR", ":9820"),
		NodeName:   getenvDefault("NODE_NAME", ""),
	}

	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address for metrics and health endpoints")
	flag.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "observed Kubernetes node name (defaults to hostname when empty)")
	flag.Parse()

	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return Config{}, fmt.Errorf("listen address must not be empty")
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
