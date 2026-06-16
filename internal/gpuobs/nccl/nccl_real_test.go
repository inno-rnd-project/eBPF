//go:build nccl

package nccl

import (
	"encoding/binary"
	"testing"
)

// makeRawEvent는 decodeNcclEvent 검증용 32 bytes wire 버퍼를 만든다. bpf/nccl_uprobe.bpf.c의
// struct nccl_event layout (ts_ns 8 + duration_ns 8 + pid 4 + tid 4 + rank_count 4 + op 1 +
// pad 3) 을 NativeEndian으로 채운다.
func makeRawEvent(durationNs uint64, rankCount uint32, op uint8) []byte {
	b := make([]byte, rawNcclEventSize)
	binary.NativeEndian.PutUint64(b[0:8], 1_000_000)   // ts_ns (monotonic, decode가 무시)
	binary.NativeEndian.PutUint64(b[8:16], durationNs) // duration_ns
	binary.NativeEndian.PutUint32(b[16:20], 1234)      // pid
	binary.NativeEndian.PutUint32(b[20:24], 5678)      // tid
	binary.NativeEndian.PutUint32(b[24:28], rankCount) // rank_count
	b[28] = op                                         // op
	return b
}

// TestDecodeNcclEvent는 wire 바이트의 duration과 rank_count와 op이 Event로 정확히 디코드되는지
// 검증한다. BPF struct layout과 Go decode의 offset 정합 회귀 가드다.
func TestDecodeNcclEvent(t *testing.T) {
	ev, ok := decodeNcclEvent(makeRawEvent(42_000_000, 8, opAllReduce))
	if !ok {
		t.Fatalf("decodeNcclEvent ok=false want true")
	}
	if ev.DurationNs != 42_000_000 {
		t.Errorf("DurationNs=%d want 42000000", ev.DurationNs)
	}
	if ev.RankCount != 8 {
		t.Errorf("RankCount=%d want 8", ev.RankCount)
	}
	if ev.Operation != "allreduce" {
		t.Errorf("Operation=%q want allreduce", ev.Operation)
	}
	if ev.Timestamp.IsZero() {
		t.Errorf("Timestamp is zero want wall-clock receive time")
	}
}

// TestDecodeNcclEvent_Short는 32 bytes 미만 버퍼가 ok=false로 거부되는지 검증한다. ringbuf wire
// 절단 시 잘못된 sample이 histogram에 끼지 않게 하는 회귀 가드다.
func TestDecodeNcclEvent_Short(t *testing.T) {
	if _, ok := decodeNcclEvent(make([]byte, rawNcclEventSize-1)); ok {
		t.Errorf("decodeNcclEvent on short buffer ok=true want false")
	}
}

// TestOperationName은 op enum이 recording rule과 dashboard 라벨로 쓰는 소문자 문자열로 매핑되고
// 미정의 값이 unknown으로 폴백하는지 검증한다. BPF enum과 Go 매핑 drift 회귀 가드다.
func TestOperationName(t *testing.T) {
	cases := map[uint8]string{
		opAllReduce:     "allreduce",
		opBroadcast:     "broadcast",
		opReduceScatter: "reducescatter",
		opAllGather:     "allgather",
		0:               "unknown",
		99:              "unknown",
	}
	for op, want := range cases {
		if got := operationName(op); got != want {
			t.Errorf("operationName(%d)=%q want %q", op, got, want)
		}
	}
}

// TestNewProduction_AvailableBeforeAttach는 Attach 전 production Profiler가 Available=false인지
// 검증한다. wire-up이 Attach 성공 후에만 gpuobs_nccl_profiler_available=1을 set하는 회귀 가드다.
func TestNewProduction_AvailableBeforeAttach(t *testing.T) {
	p := NewProduction("/nonexistent/libnccl.so.2")
	defer func() { _ = p.Close() }()
	if p.Available() {
		t.Errorf("NewProduction.Available()=true want false before Attach")
	}
}
