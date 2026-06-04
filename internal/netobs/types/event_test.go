package types

import (
	"testing"
	"unsafe"
)

// TestEventSize 는 BPF 측 netobs_event 와 Go 측 Event 구조체의 size 가 일치하는지 검증한다. BPF
// ringbuf 가 raw byte 로 전송하므로 두 size 가 다르면 corrupted parsing 위험이 있다. BPF struct 는
// common.h 에서 정의되며 #65 의 TCP 상태 메트릭 3 필드 추가로 88 → 96 byte 로 확장되었고 #83 의
// StackID 와 Pad83 추가로 96 → 104 byte 로 재확장되었다. #103 의 IPv6 확장 으로 Saddr / Daddr 가
// uint32 (4 byte) 에서 [16]byte 로 변경 되어 각각 12 byte 씩 증가, Family 1 byte 추가 하여 104 → 128
// byte 로 재확장 된다.
func TestEventSize(t *testing.T) {
	const expected = 128
	got := int(unsafe.Sizeof(Event{}))
	if got != expected {
		t.Errorf("Event size=%d want %d (BPF struct 와 layout 불일치)", got, expected)
	}
}

// TestStageName_RcvLabels 는 #65 의 rcv path 4 종 stage 가 사람 읽을 수 있는 라벨 (rcv_l3 /
// rcv_demux / rcv_established / rcv_app) 로 매핑되는지 검증한다. send path 5 종도 회귀 가드로 함께
// 확인해 추후 enum 재배치 시 매핑 흐트러짐을 막는다.
func TestStageName_RcvLabels(t *testing.T) {
	cases := []struct {
		stage uint8
		want  string
	}{
		{StageSendmsgRet, "sendmsg_ret"},
		{StageToVeth, "to_veth"},
		{StageToDevQ, "to_devq"},
		{StageRetrans, "retrans"},
		{StageDrop, "drop"},
		{StageRcvL3, "rcv_l3"},
		{StageRcvDemux, "rcv_demux"},
		{StageRcvEstablished, "rcv_established"},
		{StageRcvApp, "rcv_app"},
		// #82 send path stage 4 분해의 2 신규 enum.
		{StageTcpWriteXmit, "tcp_write_xmit"},
		{StageTcpTransmitSkb, "tcp_transmit_skb"},
		{0, "unknown"},
		{255, "unknown"},
	}
	for _, tc := range cases {
		if got := StageName(tc.stage); got != tc.want {
			t.Errorf("StageName(%d)=%q want %q", tc.stage, got, tc.want)
		}
	}
}

// TestStageDirection 은 stage 별 흐름 방향 분류가 send=egress / rcv=ingress / 기타=unknown 으로
// 맞물려 enricher 의 Direction 라벨이 빈 문자열로 빠지지 않는지 확인한다.
func TestStageDirection(t *testing.T) {
	cases := []struct {
		stage uint8
		want  string
	}{
		{StageSendmsgRet, "egress"},
		{StageToVeth, "egress"},
		{StageToDevQ, "egress"},
		{StageRetrans, "egress"},
		{StageDrop, "egress"},
		{StageRcvL3, "ingress"},
		{StageRcvDemux, "ingress"},
		{StageRcvEstablished, "ingress"},
		{StageRcvApp, "ingress"},
		// #82 신규 send path stage 2 종 의 방향 분류 (egress).
		{StageTcpWriteXmit, "egress"},
		{StageTcpTransmitSkb, "egress"},
		{0, "unknown"},
		{99, "unknown"},
	}
	for _, tc := range cases {
		if got := StageDirection(tc.stage); got != tc.want {
			t.Errorf("StageDirection(%d)=%q want %q", tc.stage, got, tc.want)
		}
	}
}

// TestIPProtocolName 은 protocol 매핑 helper 가 TCP / UDP / unknown 을 정확히 식별하는지 검증한다.
func TestIPProtocolName(t *testing.T) {
	cases := []struct {
		input uint8
		want  string
	}{
		{6, "TCP"},
		{17, "UDP"},
		{0, "unknown"},
		{1, "unknown"},  // ICMP
		{47, "unknown"}, // GRE
	}
	for _, tc := range cases {
		got := IPProtocolName(tc.input)
		if got != tc.want {
			t.Errorf("IPProtocolName(%d)=%q want %q", tc.input, got, tc.want)
		}
	}
}

// TestIPVersion 은 #103 의 ip_version 라벨 산정 진입점 회귀 가드 다. family enum 의 IPv4 / IPv6
// 외 값 (예: 0 또는 AF_UNIX 1) 은 빈 문자열 반환 으로 라벨 미부착 처리 된다.
func TestIPVersion(t *testing.T) {
	cases := []struct {
		family uint8
		want   string
	}{
		{2, "4"},   // NETOBS_AF_INET
		{10, "6"},  // NETOBS_AF_INET6
		{0, ""},    // unset
		{1, ""},    // AF_UNIX
		{17, ""},   // AF_PACKET 등 추적 대상 외
	}
	for _, tc := range cases {
		got := IPVersion(tc.family)
		if got != tc.want {
			t.Errorf("IPVersion(%d)=%q want %q", tc.family, got, tc.want)
		}
	}
}

// TestIPToString 은 #103 의 통합 슬롯 ([16]byte) IP 변환 진입점 회귀 가드 다. IPv4 는 첫 4 byte 의
// 네트워크 표현 을 net.IP 로 변환, IPv6 는 16 byte 전체 를 net.IP 로 변환, 알 수 없는 family 는
// 빈 문자열.
func TestIPToString(t *testing.T) {
	// IPv4 10.0.0.1 의 첫 4 byte raw 표현.
	v4 := [16]byte{10, 0, 0, 1}
	if got := IPToString(2, v4); got != "10.0.0.1" {
		t.Errorf("IPToString(AF_INET, 10.0.0.1)=%q want 10.0.0.1", got)
	}

	// IPv6 2001:db8::1 의 16 byte raw 표현.
	v6 := [16]byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01}
	if got := IPToString(10, v6); got != "2001:db8::1" {
		t.Errorf("IPToString(AF_INET6, 2001:db8::1)=%q want 2001:db8::1", got)
	}

	// IPv6 link-local fe80::1 (BPF 단 필터 의 대상 이지만 IPToString 자체 는 정상 표현 반환).
	v6Link := [16]byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x01}
	if got := IPToString(10, v6Link); got != "fe80::1" {
		t.Errorf("IPToString(AF_INET6, fe80::1)=%q want fe80::1", got)
	}

	// unknown family 0 은 빈 문자열.
	if got := IPToString(0, v4); got != "" {
		t.Errorf("IPToString(0, ...)=%q want empty", got)
	}
}
