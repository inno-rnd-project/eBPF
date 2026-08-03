package ebpfx

import (
	"testing"
	"unsafe"

	"netobs/internal/netobs/types"
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

// TestStartInfoSize 는 #121 의 ts_segment_entry 추가 후 netobs_start_info 가 BPF 측 정의와 정합한
// 144 byte 인지 회귀 가드 한다. #103 의 IPv6 확장 후 136 byte 에서 ts_segment_entry (u64 8 byte) 1 필드
// 추가 하여 144 byte 가 된다. 본 가드 가 깨지면 BPF C 측 struct 와 Go 측 generated struct 의 layout
// 불일치 위험 이 있으며 LRU_HASH starts map 의 marshal 길이 검증 도 함께 실패 한다.
func TestStartInfoSize(t *testing.T) {
	const want = 144
	got := int(unsafe.Sizeof(NetObsNetobsStartInfo{}))
	if got != want {
		t.Errorf("NetObsNetobsStartInfo size=%d want %d (#121 ts_segment_entry 추가 후 size 정합)", got, want)
	}
}

// TestEventSize 는 #121 의 full_latency_ns 와 segment_count 추가 후 types.Event (= BPF 측 netobs_event)
// 가 정합한 144 byte 인지 회귀 가드 한다. #103 의 IPv6 확장 후 128 byte 에서 FullLatencyNs (u64 8 byte)
// + SegmentCount (u32 4 byte) + Pad121 (4 byte) = 16 byte 증가 하여 144 byte 가 된다. 본 가드 가
// 깨지면 ringbuf event 의 binary.Read parse 가 어긋나 모든 stage 메트릭 라벨이 잘못 채워질 위험 이
// 있다.
func TestEventSize(t *testing.T) {
	const want = 144
	got := int(unsafe.Sizeof(types.Event{}))
	if got != want {
		t.Errorf("types.Event size=%d want %d (#121 full_latency_ns 추가 후 size 정합)", got, want)
	}
}

// TestFlowBytesMaxEntries 는 flow_bytes max_entries 상향 (#351 1024 → 32768, #403 32768 → 131072) 을 회귀 가드한다.
// flow_bytes 는 5-tuple 키라 노드의 모든 flow 가 슬롯을 경쟁하고 userspace FlowGuard allow-list 는
// scrape 단계에만 있어 BPF 점유를 막지 못해, 1024 에서는 관심 flow 가 노이즈에 밀려 evict 되어
// counter reset 이 반복됐다. embedded CollectionSpec 을 커널 없이 파싱해 max_entries 를 단정한다.
// 값이 되돌려지면 본 가드가 깨진다.
func TestFlowBytesMaxEntries(t *testing.T) {
	const want = 131072
	spec, err := LoadNetObs()
	if err != nil {
		t.Fatalf("LoadNetObs: %v", err)
	}
	m, ok := spec.Maps["flow_bytes"]
	if !ok {
		t.Fatal("flow_bytes map spec 부재")
	}
	if int(m.MaxEntries) != want {
		t.Errorf("flow_bytes MaxEntries=%d want %d (#351 상향 회귀)", m.MaxEntries, want)
	}
}
