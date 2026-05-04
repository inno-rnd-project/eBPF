// Package cuda 는 libcuda.so.1 의 driver API 심볼에 uprobe 를 attach 해 ringbuf 로 전달되는
// 이벤트를 PID→Pod / PID→GPU 해상도까지 끝낸 뒤 metrics 계층으로 누적시키는 모듈이다.
//
// 본 패키지는 cilium/ebpf 의 link.OpenExecutable + Uprobe 로 user-space 심볼에 BPF 프로그램을
// 붙이며, BPF 산출물(cudauprobe_bpf{el,eb}.{go,o}) 은 Makefile generate-gpuobs 가 emit 한다.
//
// lifecycle 은 Reader.Run 이 소유하고, ctx 종료 시 ringbuf reader / device map refresher /
// 모든 uprobe link / BPF object 를 graceful 해제한다.
package cuda

// rawEvent 는 BPF 측 struct cuda_event 의 wire layout 과 1:1 일치한다 (32 bytes 고정).
// binary.Read(NativeEndian) 로 디코드하므로 필드 순서 / 정렬 / 패딩이 BPF 헤더와 동기화 유지되어야 하며,
// types_test.go 의 size 검증이 실제 wire 크기와 본 구조체 크기가 일치하는지 매 빌드에서 검사한다.
type rawEvent struct {
	TsNs  uint64
	Bytes uint64
	PID   uint32
	TID   uint32
	Kind  uint8
	Pad   [7]uint8
}

// rawEventSize 는 BPF 측 struct cuda_event 의 고정 wire 크기다.
// 컴파일 단계에서 unsafe.Sizeof(rawEvent{}) 와 일치하는지 확인해 layout drift 를 차단한다.
const rawEventSize = 32
