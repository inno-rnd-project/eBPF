package cuda

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

// TestRawEventSizeMatchesBPFLayout 는 rawEvent Go 구조체의 wire 크기가 BPF C struct cuda_event 과
// #67 의 LatencyNs 필드 추가로 40 bytes 로 일치하는지 검증한다. 어느 한 쪽 필드가 추가/변경되면 본
// 테스트가 실패해 binary.Read 디코드와 BPF emit 간 layout drift 를 컴파일 단계 직후 즉시 잡는다.
func TestRawEventSizeMatchesBPFLayout(t *testing.T) {
	if got := unsafe.Sizeof(rawEvent{}); got != rawEventSize {
		t.Fatalf("unsafe.Sizeof(rawEvent)=%d want %d", got, rawEventSize)
	}
	if got := binary.Size(rawEvent{}); got != rawEventSize {
		t.Fatalf("binary.Size(rawEvent)=%d want %d", got, rawEventSize)
	}
}

// TestDecodeRawEvent_RoundTrip 는 직접 디코드 결과가 binary.Write 로 인코딩한 원본과 정확히 일치하는지 검증한다.
// 이 테스트가 깨지면 BPF wire layout 과 Go 측 디코드의 byte-offset 가 어긋난 신호다.
// DeviceOrd 가 [28:32], LatencyNs 가 [32:40] 슬롯에서 정확히 디코드되는지도 함께 검증해 multi-GPU
// attribution 과 #67 의 sync latency wire layout 변경이 silent drift 되지 않게 한다.
func TestDecodeRawEvent_RoundTrip(t *testing.T) {
	want := rawEvent{
		TsNs:      0x1122334455667788,
		Bytes:     0x99AABBCCDDEEFF00,
		PID:       0xDEADBEEF,
		TID:       0xCAFEBABE,
		Kind:      3,
		DeviceOrd: 0x12345678,
		LatencyNs: 0xAABBCCDD11223344,
	}
	buf := make([]byte, rawEventSize)
	binary.NativeEndian.PutUint64(buf[0:8], want.TsNs)
	binary.NativeEndian.PutUint64(buf[8:16], want.Bytes)
	binary.NativeEndian.PutUint32(buf[16:20], want.PID)
	binary.NativeEndian.PutUint32(buf[20:24], want.TID)
	buf[24] = want.Kind
	binary.NativeEndian.PutUint32(buf[28:32], want.DeviceOrd)
	binary.NativeEndian.PutUint64(buf[32:40], want.LatencyNs)

	got, ok := decodeRawEvent(buf)
	if !ok {
		t.Fatalf("decodeRawEvent returned ok=false on full-size input")
	}
	if got.TsNs != want.TsNs || got.Bytes != want.Bytes || got.PID != want.PID || got.TID != want.TID || got.Kind != want.Kind || got.DeviceOrd != want.DeviceOrd || got.LatencyNs != want.LatencyNs {
		t.Fatalf("decode mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestDecodeRawEvent_ShortInputReturnsNotOK 는 길이가 부족한 wire 입력을 graceful skip 하는지 검증한다.
// ringbuf 가 손상된 record 를 돌려준 경우 panic 으로 reader 루프가 죽는 것을 막는 안전망이다.
func TestDecodeRawEvent_ShortInputReturnsNotOK(t *testing.T) {
	for _, n := range []int{0, 1, rawEventSize - 1} {
		if _, ok := decodeRawEvent(make([]byte, n)); ok {
			t.Errorf("len=%d expected ok=false, got ok=true", n)
		}
	}
}
