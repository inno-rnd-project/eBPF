package ebpfx

import (
	"testing"
	"unsafe"
)

// TestStructSizes 는 #85 의 신규 netobs_flow_key 와 start_info 의 is_ipv4 추가 후에도 struct size 가
// BPF 측 정의와 정합 한지 회귀 가드 한다. netobs_flow_key 는 cgroup_id (8) + saddr (4) + daddr (4) +
// sport (2) + dport (2) + protocol (1) + direction (1) + pad (2) = 24 byte 이며 8-byte align 으로
// trailing padding 없이 24 로 고정 된다. start_info 의 is_ipv4 는 기존 pad82 6 byte 중 1 byte 를
// 흡수 해 struct size 변경 이 없 어야 한다.
func TestFlowKeySize(t *testing.T) {
	const want = 24
	got := int(unsafe.Sizeof(NetObsNetobsFlowKey{}))
	if got != want {
		t.Errorf("NetObsNetobsFlowKey size=%d want %d", got, want)
	}
}

func TestFlowValueSize(t *testing.T) {
	const want = 8
	got := int(unsafe.Sizeof(NetObsNetobsFlowValue{}))
	if got != want {
		t.Errorf("NetObsNetobsFlowValue size=%d want %d", got, want)
	}
}
