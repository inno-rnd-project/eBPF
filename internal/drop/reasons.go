package drop

import (
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	symbolRE = regexp.MustCompile(`\{\s*(0x[0-9a-fA-F]+|\d+)\s*,\s*"([^"]+)"\s*\}`)
)

type Mapper struct {
	names map[uint32]string
}

func DefaultPaths(override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{
			override,
			"/sys/kernel/debug/tracing/events/skb/kfree_skb/format",
		}
	}

	return []string{
		"/sys/kernel/tracing/events/skb/kfree_skb/format",
		"/sys/kernel/debug/tracing/events/skb/kfree_skb/format",
	}
}

func NewMapper(paths []string) *Mapper {
	m := &Mapper{
		names: make(map[uint32]string),
	}

	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if err := m.loadFromTraceFormat(p); err == nil && len(m.names) > 0 {
			log.Printf("drop reason runtime map loaded from %s (%d entries)", p, len(m.names))
			return m
		}
	}

	log.Printf("drop reason runtime map unavailable; using generic REASON_<code> fallback")
	return m
}

func (m *Mapper) loadFromTraceFormat(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	matches := symbolRE.FindAllStringSubmatch(string(b), -1)
	if len(matches) == 0 {
		return fmt.Errorf("no drop reason symbols found in %s", path)
	}

	next := make(map[uint32]string, len(matches))
	for _, sm := range matches {
		code, err := parseUint(sm[1])
		if err != nil {
			continue
		}
		name := normalizeReasonName(sm[2])
		next[code] = name
	}

	if len(next) == 0 {
		return fmt.Errorf("parsed zero symbols from %s", path)
	}

	m.names = next
	return nil
}

func parseUint(s string) (uint32, error) {
	if strings.HasPrefix(strings.ToLower(s), "0x") {
		v, err := strconv.ParseUint(s[2:], 16, 32)
		return uint32(v), err
	}

	v, err := strconv.ParseUint(s, 10, 32)
	return uint32(v), err
}

func normalizeReasonName(name string) string {
	n := strings.TrimSpace(strings.ToUpper(name))
	n = strings.TrimPrefix(n, "SKB_DROP_REASON_")
	n = strings.TrimPrefix(n, "SKB_")
	if n == "" {
		return "UNKNOWN"
	}
	return n
}

func (m *Mapper) Name(code uint32) string {
	if name, ok := m.names[code]; ok && name != "" {
		return name
	}
	return fmt.Sprintf("REASON_%d", code)
}

func (m *Mapper) Category(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))

	switch {
	case strings.Contains(n, "SOCKET"):
		return "socket"
	case strings.Contains(n, "CSUM"):
		return "checksum"
	case strings.Contains(n, "NETFILTER"),
		strings.Contains(n, "FILTER"),
		strings.Contains(n, "TC_"),
		strings.Contains(n, "XDP"):
		return "policy"
	case strings.Contains(n, "QDISC"),
		strings.Contains(n, "QUEUE"),
		strings.Contains(n, "BACKLOG"),
		strings.Contains(n, "RING"):
		return "queue"
	case strings.Contains(n, "NOMEM"),
		strings.Contains(n, "MEM"),
		strings.Contains(n, "FULL_RING"):
		return "resource"
	case strings.Contains(n, "ROUTE"),
		strings.Contains(n, "NOROUTES"),
		strings.Contains(n, "RPFILTER"),
		strings.Contains(n, "NEIGH"):
		return "routing"
	case strings.Contains(n, "PROTO"),
		strings.Contains(n, "IP_"),
		strings.Contains(n, "PKT_"),
		strings.Contains(n, "HDR"):
		return "protocol"
	case strings.Contains(n, "TAP"),
		strings.Contains(n, "DEV_"),
		strings.Contains(n, "OTHERHOST"):
		return "device"
	default:
		return "unknown"
	}
}

func (m *Mapper) Describe(code uint32) (string, string) {
	name := m.Name(code)
	return name, m.Category(name)
}
