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

	// PodFlowDstEnabled는 stage/drop/retrans 메트릭에 dst_namespace/dst_workload 라벨을 emit할지를
	// 토글한다. false면 두 라벨 모두 빈 문자열로 채워져 cardinality가 도입 전과 동일하게 유지된다.
	PodFlowDstEnabled bool

	// PodFlowDstUIDAllowNamespaces는 dst_pod_uid 라벨이 emit되는 namespace 화이트리스트다. 빈 슬라이스가
	// 기본이며, 그 경우 모든 시리즈에서 dst_pod_uid가 빈 문자열로 emit된다. 등록된 namespace의 dst Pod
	// 식별 흐름에 한해 UID가 채워져 카디널리티 폭발을 막는다.
	PodFlowDstUIDAllowNamespaces []string
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

// parseNamespaceList는 콤마 구분 namespace 문자열을 정규화된 슬라이스로 변환한다. 공백 trim과 빈
// 토큰 제거, 중복 dedup을 수행해 startup 시점 1회 결정으로 운영자가 안전하게 multi-line yaml 또는
// env로 주입 가능하게 한다. 입력에 등장한 첫 occurrence 순서를 보존해 운영자가 의도한 우선순위
// (예: 자주 매칭되는 namespace 를 앞쪽에 배치하는 휴리스틱) 가 출력에 그대로 반영되며, 단위 테스트
// 도 본 순서 가정을 가드한다.
func parseNamespaceList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0)
	for _, tok := range strings.Split(raw, ",") {
		ns := strings.TrimSpace(tok)
		if ns == "" {
			continue
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
		TargetIP:                     getenv("TARGET_IP", ""),
		ListenAddr:                   getenv("LISTEN_ADDR", ":9810"),
		PrintEvents:                  getenvBool("PRINT_EVENTS", false),
		PodMetricsEnabled:            getenvBool("POD_METRICS_ENABLED", true),
		NodeName:                     getenv("NODE_NAME", hostnameOr("unknown-node")),
		MetadataRefresh:              metadataRefresh,
		DropReasonFormatPath:         getenv("DROP_REASON_FORMAT_PATH", "/sys/kernel/tracing/events/skb/kfree_skb/format"),
		PodFlowDstEnabled:            getenvBool("POD_FLOW_DST_ENABLED", true),
		PodFlowDstUIDAllowNamespaces: parseNamespaceList(getenv("POD_FLOW_DST_UID_ALLOW_NAMESPACES", "")),
	}

	fs := flag.NewFlagSet("netobs-agent", flag.ContinueOnError)
	fs.StringVar(&cfg.TargetIP, "target-ip", cfg.TargetIP, "destination Pod IPv4 to trace; empty means observe all")
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address for /metrics, /healthz, /readyz")
	fs.BoolVar(&cfg.PrintEvents, "print-events", cfg.PrintEvents, "print events to stdout")
	fs.BoolVar(&cfg.PodMetricsEnabled, "pod-metrics", cfg.PodMetricsEnabled, "emit per-pod-instance metrics (netobs_pod_stage_*); disable on large clusters to cap Prometheus cardinality")
	fs.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "observed Kubernetes node name")
	fs.DurationVar(&cfg.MetadataRefresh, "metadata-refresh", cfg.MetadataRefresh, "Kubernetes metadata refresh interval")
	fs.StringVar(&cfg.DropReasonFormatPath, "drop-reason-format", cfg.DropReasonFormatPath, "skb:kfree_skb tracepoint format path")
	fs.BoolVar(&cfg.PodFlowDstEnabled, "pod-flow-dst", cfg.PodFlowDstEnabled, "emit dst_namespace/dst_workload labels on stage/drop/retrans metrics; disable to keep pre-flow-dst cardinality")
	var dstUIDNs string
	if len(cfg.PodFlowDstUIDAllowNamespaces) > 0 {
		dstUIDNs = strings.Join(cfg.PodFlowDstUIDAllowNamespaces, ",")
	}
	fs.StringVar(&dstUIDNs, "pod-flow-dst-uid-allow-namespaces", dstUIDNs, "comma-separated namespace allow-list whose Pods receive dst_pod_uid labels; empty disables UID emit cluster-wide")
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

	// CLI flag로 들어온 dst UID allow-list 문자열을 env와 동일한 정규화 (trim/dedup/empty-drop) 로
	// 통과시킨다. env-only 경로와 flag-override 경로가 같은 정상화 규칙을 공유해 운영 surface가
	// 일관되게 동작한다.
	cfg.PodFlowDstUIDAllowNamespaces = parseNamespaceList(dstUIDNs)

	return cfg, nil
}
