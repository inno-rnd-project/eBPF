// Package cuda 는 libcuda.so.1 의 driver API 심볼에 uprobe 를 attach 해 ringbuf 로 전달되는
// 이벤트를 PID→Pod / PID→GPU 해상도까지 끝낸 뒤 metrics 계층으로 누적시키는 모듈이다.
//
// 본 패키지는 cilium/ebpf 의 link.OpenExecutable + Uprobe 로 user-space 심볼에 BPF 프로그램을
// 붙이며, BPF 산출물(cudauprobe_bpf{el,eb}.{go,o}) 은 Makefile generate-gpuobs 가 emit 한다.
//
// lifecycle 은 Reader.Run 이 소유하고, ctx 종료 시 ringbuf reader / device map refresher /
// 모든 uprobe link / BPF object 를 graceful 해제한다.
package cuda

// rawEvent 는 BPF 측 struct cuda_event 의 wire layout 과 1:1 일치한다 (#67 의 LatencyNs 필드 추가로
// 40 bytes 로 확장됨). binary.Read(NativeEndian) 로 디코드하므로 필드 순서 / 정렬 / 패딩이 BPF
// 헤더와 동기화 유지되어야 하며, types_test.go 의 size 검증이 실제 wire 크기와 본 구조체 크기가
// 일치하는지 매 빌드에서 검사한다.
type rawEvent struct {
	TsNs      uint64
	Bytes     uint64
	PID       uint32
	TID       uint32
	Kind      uint8
	Pad       [3]uint8
	DeviceOrd uint32
	// LatencyNs 는 #67 의 동기화 wait 시간이다. STREAM_SYNC / EVENT_SYNC kind 만 entry-exit 페어로
	// 산정한 ns 값을 채우고 다른 kind 는 0 으로 둔다.
	LatencyNs uint64
}

// rawEventSize 는 BPF 측 struct cuda_event 의 고정 wire 크기다.
// 컴파일 단계에서 unsafe.Sizeof(rawEvent{}) 와 일치하는지 확인해 layout drift 를 차단한다.
const rawEventSize = 40

// CudaDeviceOrdUnknown 은 BPF 측 CUDA_DEVICE_ORD_UNKNOWN 과 동일한 sentinel 값이다.
// cuda_tid_device 에 매핑이 없을 때 emit_event 가 본 값을 device_ord 에 채워 발행하며,
// dispatch 가 본 값을 발견하면 PID-level devmap.lookup 폴백 경로로 분기한다 (#33).
const CudaDeviceOrdUnknown uint32 = 0xFFFFFFFF
