package config

import (
	"flag"
	"log"
	"net"
	"os"
	"strings"
)

type Config struct {
	TargetIP    string
	ListenAddr  string
	PrintEvents bool
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

func Parse() Config {
	cfg := Config{
		TargetIP:    getenv("TARGET_IP", ""),
		ListenAddr:  getenv("LISTEN_ADDR", ":9810"),
		PrintEvents: getenvBool("PRINT_EVENTS", true),
	}

	flag.StringVar(&cfg.TargetIP, "target-ip", cfg.TargetIP, "destination Pod IPv4 to trace; empty means no filter")
	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address for /metrics")
	flag.BoolVar(&cfg.PrintEvents, "print-events", cfg.PrintEvents, "print events to stdout")
	flag.Parse()

	if cfg.TargetIP != "" && net.ParseIP(cfg.TargetIP).To4() == nil {
		log.Fatalf("invalid -target-ip: %s", cfg.TargetIP)
	}

	return cfg
}
