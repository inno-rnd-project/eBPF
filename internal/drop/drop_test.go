package drop

import (
	"testing"
)

// -------------------------------------------------------------------
// normalizeReasonName (unexported)
// -------------------------------------------------------------------

func TestNormalizeReasonName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		// SKB_DROP_REASON_ prefix 제거
		{"SKB_DROP_REASON_NOT_SPECIFIED", "NOT_SPECIFIED"},
		{"SKB_DROP_REASON_NO_SOCKET", "NO_SOCKET"},
		{"SKB_DROP_REASON_TCP_CSUM", "TCP_CSUM"},
		// SKB_ prefix 제거 (SKB_DROP_REASON_ 없는 경우)
		{"SKB_NOT_SPECIFIED", "NOT_SPECIFIED"},
		// 앞뒤 공백 제거 + 대문자화
		{"  skb_drop_reason_socket  ", "SOCKET"},
		{"skb_not_specified", "NOT_SPECIFIED"},
		// prefix 없는 이름은 그대로 대문자화
		{"NO_PREFIX", "NO_PREFIX"},
		{"tcp_csum_error", "TCP_CSUM_ERROR"},
		// 빈 문자열 → "UNKNOWN"
		{"", "UNKNOWN"},
		{"   ", "UNKNOWN"},
	}

	for _, c := range cases {
		got := normalizeReasonName(c.input)
		if got != c.want {
			t.Errorf("normalizeReasonName(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// -------------------------------------------------------------------
// Mapper.Category
// -------------------------------------------------------------------

func TestCategory(t *testing.T) {
	m := &Mapper{}

	cases := []struct {
		name string
		want string
	}{
		// socket 카테고리
		{"NO_SOCKET", "socket"},
		{"SOCKET_RCVBUF_FULL", "socket"},
		// checksum 카테고리
		{"TCP_CSUM", "checksum"},
		{"CSUM_ERROR", "checksum"},
		// policy 카테고리
		{"NETFILTER_DROP", "policy"},
		{"FILTER_REJECT", "policy"},
		{"TC_INGRESS", "policy"},
		{"XDP_DROP", "policy"},
		// queue 카테고리
		{"QDISC_DROP", "queue"},
		{"QUEUE_PURGE", "queue"},
		{"BACKLOG_FULL", "queue"},
		{"RING_BUFFER_FULL", "queue"},
		// resource 카테고리
		{"NOMEM", "resource"},
		{"MEM_ALLOC_FAIL", "resource"},
		// routing 카테고리 (RPFILTER는 "FILTER" 때문에 policy로 분류됨에 유의)
		{"ROUTE_MISS", "routing"},
		{"NOROUTES", "routing"},
		{"NEIGH_UNRESOLVED", "routing"},
		// protocol 카테고리
		{"PROTO_UNREACH", "protocol"},
		{"IP_OUTNOROUTES", "routing"}, // "ROUTE" 포함으로 routing에 해당
		{"PKT_TOO_SMALL", "protocol"},
		{"HDR_PARSE_FAIL", "protocol"},
		// device 카테고리 (FILTER가 없는 TAP 이름 사용)
		{"TAP_DROP", "device"},
		{"DEV_READY_FAIL", "device"},
		{"OTHERHOST", "device"},
		// unknown 카테고리 (어느 패턴에도 해당 없음)
		{"UNKNOWN", "unknown"},
		{"REASON_42", "unknown"},
	}

	for _, c := range cases {
		got := m.Category(c.name)
		if got != c.want {
			t.Errorf("Category(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
