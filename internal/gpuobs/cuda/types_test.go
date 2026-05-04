package cuda

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

// TestRawEventSizeMatchesBPFLayout 는 rawEvent Go 구조체의 wire 크기가 BPF C struct cuda_event 과
// 정확히 32 bytes 로 일치하는지 검증한다. 어느 한 쪽 필드가 추가/변경되면 본 테스트가 실패해
// binary.Read 디코드와 BPF emit 간 layout drift 를 컴파일 단계 직후 즉시 잡는다.
func TestRawEventSizeMatchesBPFLayout(t *testing.T) {
	if got := unsafe.Sizeof(rawEvent{}); got != rawEventSize {
		t.Fatalf("unsafe.Sizeof(rawEvent)=%d want %d", got, rawEventSize)
	}
	if got := binary.Size(rawEvent{}); got != rawEventSize {
		t.Fatalf("binary.Size(rawEvent)=%d want %d", got, rawEventSize)
	}
}
