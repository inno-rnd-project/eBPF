package ebpfx

import (
	"testing"
	"unsafe"
)

// TestFlowKeySize 는 #103 의 IPv6 확장 후 netobs_flow_key 가 BPF 측 정의와 정합한 48 byte 인지
// 회귀 가드한다. cgroup_id (8) + saddr (16) + daddr (16) + sport (2) + dport (2) + protocol (1) +
// direction (1) + family (1) + pad (1) = 48 byte 이며 8-byte align 으로 trailing padding 없이
// 48 로 고정된다. #85 의 IPv4 한정 24 byte 에서 IPv6 통합 으로 24 byte 증가.
func TestFlowKeySize(t *testing.T) {
	const want = 48
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

// TestStartInfoSize 는 #103 의 IPv6 확장 후 netobs_start_info 가 BPF 측 정의와 정합한 136 byte 인지
// 회귀 가드 한다. #85 의 112 byte 에서 saddr / daddr 각각 12 byte 씩 (총 24 byte) 증가 하여 136 byte.
// is_ipv4 는 family (동일 1 byte) 로 의미 확장 되어 size 영향 없음. 본 가드 가 깨지면 BPF C 측 struct
// 와 Go 측 generated struct 의 layout 불일치 위험 이 있으며 LRU_HASH starts map 의 marshal 길이 검증
// 도 함께 실패 한다.
func TestStartInfoSize(t *testing.T) {
	const want = 136
	got := int(unsafe.Sizeof(NetObsNetobsStartInfo{}))
	if got != want {
		t.Errorf("NetObsNetobsStartInfo size=%d want %d (#103 IPv6 확장 후 size 정합)", got, want)
	}
}
