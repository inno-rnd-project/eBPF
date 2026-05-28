package ebpfx

import (
	"testing"
	"unsafe"
)

// TestFlowKeySize 는 #85 의 신규 netobs_flow_key 가 BPF 측 정의와 정합한 24 byte 인지 회귀 가드한다.
// cgroup_id (8) + saddr (4) + daddr (4) + sport (2) + dport (2) + protocol (1) + direction (1) + pad
// (2) = 24 byte 이며 8-byte align으로 trailing padding 없이 24로 고정된다.
func TestFlowKeySize(t *testing.T) {
	const want = 24
	got := int(unsafe.Sizeof(NetObsNetobsFlowKey{}))
	if got != want {
		t.Errorf("NetObsNetobsFlowKey size=%d want %d", got, want)
	}
}

// TestFlowValueSize 는 신규 netobs_flow_value 가 BPF 측 정의의 단일 u64 bytes 필드와 정합한 8 byte
// 인지 회귀 가드한다.
func TestFlowValueSize(t *testing.T) {
	const want = 8
	got := int(unsafe.Sizeof(NetObsNetobsFlowValue{}))
	if got != want {
		t.Errorf("NetObsNetobsFlowValue size=%d want %d", got, want)
	}
}

// TestStartInfoSize 는 #85 의 is_ipv4 1 byte 추가 후에도 netobs_start_info 의 struct size 가 변경되지
// 않는지 회귀 가드한다. 기존 pad82 6 byte 중 1 byte 를 is_ipv4 로 흡수해 layout이 동일하게 유지된다.
// 본 가드 가 깨지면 BPF C 측 struct 와 Go 측 generated struct 의 layout 불일치 위험이 있으며 LRU_HASH
// starts map 의 marshal 길이 검증도 함께 실패한다.
func TestStartInfoSize(t *testing.T) {
	const want = 112
	got := int(unsafe.Sizeof(NetObsNetobsStartInfo{}))
	if got != want {
		t.Errorf("NetObsNetobsStartInfo size=%d want %d (is_ipv4 추가 후에도 기존 size 유지 회귀 가드)", got, want)
	}
}
