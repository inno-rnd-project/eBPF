package drop

import "testing"

// TestStage 는 #197 의 drop reason → 경로 단계 귀속을 검증한다. dev 커널 kfree_skb format 에 실재하는
// reason (TC_INGRESS / TC_EGRESS / QDISC_DROP / QUEUE_PURGE / TCP_OFO_DROP / NOT_SPECIFIED) 로 케이스를
// 고정해 송신/수신 경로 단계가 정확히 분류되는지 확인한다.
func TestStage(t *testing.T) {
	m := &Mapper{}
	cases := []struct {
		name string
		want string
	}{
		// 프로덕션 enricher 는 mapper.Describe 가 정규화한 (SKB_DROP_REASON_ 접두사 제거) 이름을
		// Stage 에 넘기므로, dev drop_reason 라벨과 동일한 정규화형으로 케이스를 고정한다.
		{"TC_INGRESS", "ingress_tc"},
		{"TC_EGRESS", "egress_tc"},
		{"QDISC_DROP", "egress_qdisc"},
		{"QUEUE_PURGE", "egress_qdisc"},
		{"TCP_OFO_DROP", "recv_reorder"},
		{"TCP_OLD_DATA", "recv_reorder"},
		{"TCP_INVALID_SEQUENCE", "recv_tcp"},
		{"NO_SOCKET", "socket"},
		{"IP_OUTNOROUTES", "routing"},
		{"XDP", "ingress_early"},
		{"PKT_TOO_SMALL", "protocol"},
		{"NOT_SPECIFIED", "unknown"},
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := m.Stage(c.name); got != c.want {
			t.Errorf("Stage(%q)=%q want %q", c.name, got, c.want)
		}
	}
}

// TestCategory_SocketTokenBoundary 는 #145 의 SOCK / SOCKET 인접 토큰 오분류 회귀 가드다. 부분문자열
// 매칭이 "SOCKET" 을 찾지 못해 PACKET_SOCK_ERROR (토큰 SOCK) 가 unknown 으로 빠지던 버그가 토큰 경계
// 매칭으로 socket 으로 분류되는지, 기존 SOCKET 토큰 reason 의 socket 정분류가 유지되는지 검증한다.
// dev 커널 (6.8.0-60) 의 kfree_skb format 에 실재하는 reason 으로 케이스를 고정한다.
func TestCategory_SocketTokenBoundary(t *testing.T) {
	m := &Mapper{}
	cases := []struct {
		name string
		want string
	}{
		// #145 의 핵심 오분류 케이스. SOCK 토큰이라 부분문자열 "SOCKET" 으로는 못 잡혔다.
		{"PACKET_SOCK_ERROR", "socket"},
		// 기존 SOCKET 토큰 reason 의 socket 정분류 유지 회귀 가드.
		{"NO_SOCKET", "socket"},
		{"SOCKET_FILTER", "socket"},
		{"SOCKET_BACKLOG", "socket"},
		{"SOCKET_RCVBUFF", "socket"},
	}
	for _, tc := range cases {
		if got := m.Category(tc.name); got != tc.want {
			t.Errorf("Category(%q)=%q want %q", tc.name, got, tc.want)
		}
	}
}

// TestCategory_OtherCategories 는 socket 외 카테고리의 대표 reason 분류가 SOCK/SOCKET 보정으로 회귀
// 하지 않는지 가드한다. 부분문자열 매칭과 switch 순서에 의존하는 기존 동작을 그대로 고정한다.
func TestCategory_OtherCategories(t *testing.T) {
	m := &Mapper{}
	cases := []struct {
		name string
		want string
	}{
		{"IP_CSUM", "checksum"},
		{"TCP_CSUM", "checksum"},
		{"NETFILTER_DROP", "policy"},
		{"XDP", "policy"},
		{"QUEUE_PURGE", "queue"},
		{"NOMEM", "resource"},
		{"NEIGH_FAILED", "routing"},
		{"PKT_TOO_BIG", "protocol"},
		{"UNHANDLED_PROTO", "protocol"},
		{"DEV_READY", "device"},
		// 해당 카테고리가 없어 unknown 으로 유지되는 reason (카테고리 종류 재설계는 #145 비목표).
		{"TCP_CLOSE", "unknown"},
		{"NOT_SPECIFIED", "unknown"},
	}
	for _, tc := range cases {
		if got := m.Category(tc.name); got != tc.want {
			t.Errorf("Category(%q)=%q want %q", tc.name, got, tc.want)
		}
	}
}

// TestHasToken 은 토큰 경계 helper 의 동작을 직접 가드한다. 토큰 정확 일치만 true 이고 부분문자열
// (SOCKET 의 일부인 SOCK 등) 은 false 임을 고정한다.
func TestHasToken(t *testing.T) {
	if !hasToken("PACKET_SOCK_ERROR", "SOCK", "SOCKET") {
		t.Errorf("hasToken(PACKET_SOCK_ERROR, SOCK/SOCKET)=false want true")
	}
	if !hasToken("NO_SOCKET", "SOCK", "SOCKET") {
		t.Errorf("hasToken(NO_SOCKET, SOCK/SOCKET)=false want true")
	}
	// "SOCKET" 토큰은 "SOCK" 토큰과 다르다. SOCK 만 찾으면 SOCKET 토큰은 매칭되지 않아야 한다.
	if hasToken("NO_SOCKET", "SOCK") {
		t.Errorf("hasToken(NO_SOCKET, SOCK)=true want false (SOCKET 토큰은 SOCK 토큰이 아님)")
	}
	if hasToken("TCP_CLOSE", "SOCK", "SOCKET") {
		t.Errorf("hasToken(TCP_CLOSE, SOCK/SOCKET)=true want false")
	}
}
