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

// hasToken 은 reason 이름을 '_' 토큰으로 분해해 정확한 토큰 일치를 검사한다. 부분문자열 매칭이
// SOCK 과 SOCKET 같은 인접 토큰을 구분하지 못해 PACKET_SOCK_ERROR (토큰 SOCK) 가 socket 으로 분류
// 되지 못하고 unknown 으로 빠지던 오분류를 제거한다. SOCK 또는 SOCKET 토큰을 가진 reason 은
// 모두 socket 으로 분류된다.
//
// Category 는 drop 이벤트마다 호출되는 hot path 라 strings.Split 의 슬라이스 힙 할당을 피하고
// strings.IndexByte 로 토큰 경계를 순회한다. Go 의 문자열 슬라이싱은 backing array 를 공유해 추가
// 할당이 없으므로 본 구현은 zero-allocation 이다.
func hasToken(name string, tokens ...string) bool {
	s := name
	for {
		idx := strings.IndexByte(s, '_')
		tok := s
		if idx >= 0 {
			tok = s[:idx]
		}
		for _, want := range tokens {
			if tok == want {
				return true
			}
		}
		if idx < 0 {
			return false
		}
		s = s[idx+1:]
	}
}

func (m *Mapper) Category(name string) string {
	n := strings.ToUpper(strings.TrimSpace(name))

	switch {
	case hasToken(n, "SOCK", "SOCKET"):
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
