package types

import (
	"testing"
	"unsafe"
)

// TestEventSize 는 BPF 측 netobs_event 와 Go 측 Event 구조체의 size 가 일치하는지 검증한다. BPF
// ringbuf 가 raw byte 로 전송하므로 두 size 가 다르면 corrupted parsing 위험이 있다. BPF struct
// 는 common.h 에서 8 byte 정렬 기준 96 byte 로 fixed 다.
func TestEventSize(t *testing.T) {
	const expected = 88
	got := int(unsafe.Sizeof(Event{}))
	if got != expected {
		t.Errorf("Event size=%d want %d (BPF struct 와 layout 불일치)", got, expected)
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
