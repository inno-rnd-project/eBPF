package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dnsLabelRE는 Kubernetes namespace 이름이 따르는 RFC1123 DNS 라벨 규칙이다. 소문자 영숫자와
// 하이픈으로 구성되며 첫/마지막 글자는 영숫자여야 한다. allow-list 에 들어온 namespace 이름을 본
// 패턴으로 검증해 운영자가 오타 (대문자, 언더스코어, 공백 등) 로 silent miss 를 만드는 상황을
// startup 시점에 차단한다.
var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validateNamespaceName은 RFC1123 DNS 라벨 규칙 위반 시 명시적 에러를 반환한다. 길이 한계 63 자도
// 함께 강제해 운영자가 잘못된 입력을 fail-fast 로 알 수 있게 한다.
func validateNamespaceName(ns string) error {
	if len(ns) == 0 {
		return errors.New("namespace name must not be empty")
	}
	if len(ns) > 63 {
		return fmt.Errorf("namespace name %q exceeds 63 chars", ns)
	}
	if !dnsLabelRE.MatchString(ns) {
		return fmt.Errorf("namespace name %q must match RFC1123 DNS label (lowercase alphanumerics and hyphens, start/end with alphanumeric)", ns)
	}
	return nil
}

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

	// DropFlowAllowNamespaces 는 netobs_drop_events_flow_total 의 5-tuple 메트릭이 emit 되는 src
	// namespace 화이트리스트다 (#64). 본 메트릭은 src_ip / src_port / dst_ip / dst_port / protocol 5
	// 라벨로 카디널리티 위험이 크다. 빈 슬라이스가 기본이며 그 경우 emit 자체가 일어나지 않는다.
	// 운영자가 진단 대상 namespace 만 명시 등록해 series 폭주를 차단한다.
	DropFlowAllowNamespaces []string

	// DropFlowMaxActive 는 활성 5-tuple flow 의 동시 emit 상한이다. LRU sampling 으로 본 한도를 초과
	// 하는 신규 flow 는 가장 오래된 flow 가 evict 된 후 등록된다. emit 되는 series 의 절대 상한은
	// DropFlowMaxActive * (drop_reason 수 8 종) 으로 추정된다. 기본 1024.
	DropFlowMaxActive int

	// DropStackAllowNamespaces 는 #83 의 netobs_drop_stack_total 메트릭이 emit 되는 src namespace
	// 화이트리스트다. 본 메트릭은 stack_hash 와 top_function 라벨이 추가돼 cardinality 위험이 크다.
	// 빈 슬라이스가 기본이며 그 경우 emit 자체가 일어나지 않는다 (cardinality 안전 default).
	DropStackAllowNamespaces []string

	// DropStackMaxActive 는 stack 메트릭의 활성 5-tuple flow 동시 emit 상한이다. DropFlowMaxActive 와
	// admit 결과가 독립이라 별도 cap 으로 분리한다. 기본 1024.
	DropStackMaxActive int

	// KallsymsPath 는 #83 의 userspace symbol resolver 가 파싱하는 /proc/kallsyms 경로다. 컨테이너
	// hostPath 마운트의 위치가 변경되는 경우에 한해 override 한다. 기본은 /proc/kallsyms.
	KallsymsPath string

	// NICCapacityBytesPerSec는 노드의 NIC 이론 capacity (bytes/sec) 다. correlation 진단의 network
	// throughput score 정규화 분모로 사용되는 netobs_node_nic_capacity_bytes_per_sec 메트릭의 값을
	// 결정한다. default 1.25e9 (10 GbE) 가 일반 서버 NIC와 정합하고, 운영자는 노드별 실제 NIC 사양에
	// 맞춰 env / flag 로 override 한다.
	NICCapacityBytesPerSec float64
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

// getenvFloat는 환경 변수 값을 float64로 파싱한다. 빈 값이면 default를 반환하고 파싱 실패 시 default
// 와 함께 명시적 에러를 반환한다. caller가 err를 확인하지 않고 첫 반환값만 사용하더라도 의도된 default
// 로 폴백되어 동작이 망가지지 않으며 gpuobs의 getenvFloat / getenvDuration 패턴과도 일관된다.
// correlation tunable (NIC capacity 등) 의 env 진입점이다.
func getenvFloat(key string, def float64) (float64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def, fmt.Errorf("invalid float for %s: %q", key, v)
	}
	return f, nil
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
	tokens := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
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

	nicCapacity, err := getenvFloat("NIC_CAPACITY_BYTES_PER_SEC", 1.25e9)
	if err != nil {
		return Config{}, err
	}

	dropFlowMaxActive, err := strconv.Atoi(getenv("NETOBS_DROP_FLOW_MAX_ACTIVE", "1024"))
	if err != nil || dropFlowMaxActive <= 0 {
		dropFlowMaxActive = 1024
	}

	dropStackMaxActive, err := strconv.Atoi(getenv("NETOBS_DROP_STACK_MAX_ACTIVE", "1024"))
	if err != nil || dropStackMaxActive <= 0 {
		dropStackMaxActive = 1024
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
		DropFlowAllowNamespaces:      parseNamespaceList(getenv("NETOBS_DROP_FLOW_ALLOW_NAMESPACES", "")),
		DropFlowMaxActive:            dropFlowMaxActive,
		DropStackAllowNamespaces:     parseNamespaceList(getenv("NETOBS_DROP_STACK_ALLOW_NAMESPACES", "")),
		DropStackMaxActive:           dropStackMaxActive,
		KallsymsPath:                 getenv("NETOBS_KALLSYMS_PATH", "/proc/kallsyms"),
		NICCapacityBytesPerSec:       nicCapacity,
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
	var dropFlowNs string
	if len(cfg.DropFlowAllowNamespaces) > 0 {
		dropFlowNs = strings.Join(cfg.DropFlowAllowNamespaces, ",")
	}
	fs.StringVar(&dropFlowNs, "drop-flow-allow-namespaces", dropFlowNs, "comma-separated namespace allow-list for netobs_drop_events_flow_total 5-tuple emit (#64); empty disables emit cluster-wide")
	fs.IntVar(&cfg.DropFlowMaxActive, "drop-flow-max-active", cfg.DropFlowMaxActive, "LRU sampling cap for concurrent active 5-tuple flows in drop flow metric (#64). Older flows are evicted when limit exceeded")
	var dropStackNs string
	if len(cfg.DropStackAllowNamespaces) > 0 {
		dropStackNs = strings.Join(cfg.DropStackAllowNamespaces, ",")
	}
	fs.StringVar(&dropStackNs, "drop-stack-allow-namespaces", dropStackNs, "comma-separated namespace allow-list for netobs_drop_stack_total kernel stack metric (#83); empty disables emit cluster-wide")
	fs.IntVar(&cfg.DropStackMaxActive, "drop-stack-max-active", cfg.DropStackMaxActive, "LRU sampling cap for concurrent active 5-tuple flows in drop stack metric (#83). Independent of -drop-flow-max-active")
	fs.StringVar(&cfg.KallsymsPath, "kallsyms-path", cfg.KallsymsPath, "path to /proc/kallsyms for the drop stack userspace symbol resolver (#83); only override when hostPath mount target differs")
	fs.Float64Var(&cfg.NICCapacityBytesPerSec, "nic-capacity-bytes", cfg.NICCapacityBytesPerSec, "node NIC theoretical capacity in bytes/sec; exposed as netobs_node_nic_capacity_bytes_per_sec for correlation network throughput score (default 1.25e9 = 10 GbE)")
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

	if cfg.NICCapacityBytesPerSec <= 0 {
		return Config{}, fmt.Errorf("invalid -nic-capacity-bytes: must be > 0")
	}

	// CLI flag로 들어온 dst UID allow-list 문자열을 env와 동일한 정규화 (trim/dedup/empty-drop) 로
	// 통과시킨다. env-only 경로와 flag-override 경로가 같은 정상화 규칙을 공유해 운영 surface가
	// 일관되게 동작한다.
	cfg.PodFlowDstUIDAllowNamespaces = parseNamespaceList(dstUIDNs)
	cfg.DropFlowAllowNamespaces = parseNamespaceList(dropFlowNs)
	cfg.DropStackAllowNamespaces = parseNamespaceList(dropStackNs)

	// allow-list 각 entry 가 RFC1123 DNS 라벨 규칙에 맞는지 startup 시점에 fail-fast 로 검증한다.
	// 잘못된 이름 (예: 대문자, 언더스코어 오타) 은 lookup 단계에서 silent miss 가 되어 dst_pod_uid
	// 가 emit 안 되는 디버깅 어려운 상황을 만드므로 본 검증으로 즉시 알린다.
	for _, ns := range cfg.PodFlowDstUIDAllowNamespaces {
		if err := validateNamespaceName(ns); err != nil {
			return Config{}, fmt.Errorf("invalid -pod-flow-dst-uid-allow-namespaces entry: %w", err)
		}
	}
	for _, ns := range cfg.DropFlowAllowNamespaces {
		if err := validateNamespaceName(ns); err != nil {
			return Config{}, fmt.Errorf("invalid -drop-flow-allow-namespaces entry: %w", err)
		}
	}
	for _, ns := range cfg.DropStackAllowNamespaces {
		if err := validateNamespaceName(ns); err != nil {
			return Config{}, fmt.Errorf("invalid -drop-stack-allow-namespaces entry: %w", err)
		}
	}
	if cfg.DropFlowMaxActive <= 0 {
		return Config{}, fmt.Errorf("drop-flow-max-active must be positive, got %d", cfg.DropFlowMaxActive)
	}
	if cfg.DropStackMaxActive <= 0 {
		return Config{}, fmt.Errorf("drop-stack-max-active must be positive, got %d", cfg.DropStackMaxActive)
	}

	return cfg, nil
}
