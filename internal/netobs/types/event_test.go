package types

import (
	"testing"
	"unsafe"
)

// TestEventSize 는 BPF 측 netobs_event 와 Go 측 Event 구조체의 size 가 일치하는지 검증한다. BPF
// ringbuf 가 raw byte 로 전송하므로 두 size 가 다르면 corrupted parsing 위험이 있다. BPF struct
// 는 common.h 에서 정의되며 #65 의 TCP 상태 메트릭 3 필드 추가로 88 → 96 byte 로 확장되었다.
func TestEventSize(t *testing.T) {
	const expected = 96
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
