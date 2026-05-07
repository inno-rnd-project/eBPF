package cuda

import (
	"strconv"
	"testing"
	"time"

	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// slowResolver 는 실제 kube.Resolver.ResolvePID 가 /proc/<pid>/cgroup 을 read + parse 하는
// I/O 비용을 마이크로벤치에서 재현하기 위한 고정 지연 fake 다. 기본 30µs 지연으로 사용하며,
// 본 수치는 dev 노드에서 cgroup 파일 read + 라인 파싱이 통상적으로 보이는 범위의 중앙값이다.
// 캐시 적용 후 lookup 경로의 ns/op 가 본 지연을 상수배 단축해야 한다는 비교 기준선이 된다.
type slowResolver struct {
	table map[uint32]kube.PodIdentity
	delay time.Duration
}

func (s slowResolver) ResolvePID(pid uint32) kube.PodIdentity {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if id, ok := s.table[pid]; ok {
		return id
	}
	return kube.PodIdentity{}
}

// noopRecorder 는 metrics.RecordCudaEvent 호출 자체를 무비용으로 만들어 dispatch 의 ResolvePID
// + devmap.lookup + sample 구성 비용만 측정 대상으로 남긴다.
func noopRecorder(string, metrics.CudaEventSample) {}

func newReaderForBench(resolver PodResolver) *Reader {
	r := New("/unused", "", "node-A", nil, resolver, 0)
	r.recordEvent = noopRecorder
	return r
}

// BenchmarkReaderDispatch_NilResolver 는 ResolvePID 자체를 건너뛰는 경로의 ns/op 하한이다.
// dispatch 안의 devmap.lookup + sample 구성 + recordEvent 호출 비용만 남는다.
func BenchmarkReaderDispatch_NilResolver(b *testing.B) {
	r := newReaderForBench(nil)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1234: "GPU-A"})
	raw := rawEvent{PID: 1234, Bytes: 4096, Kind: uint8(types.CudaEventH2D)}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.dispatch(raw, devmap)
	}
}

// BenchmarkReaderDispatch_FakeResolver 는 in-memory map lookup 만 수행하는 fake resolver 경로다.
// 실제 ResolvePID 의 I/O 비용을 제외한 dispatch 자체의 순수 오버헤드 상한을 측정한다.
func BenchmarkReaderDispatch_FakeResolver(b *testing.B) {
	resolver := fakeResolver{table: map[uint32]kube.PodIdentity{
		1234: samplePod("ml", "p", "u"),
	}}
	r := newReaderForBench(resolver)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1234: "GPU-A"})
	raw := rawEvent{PID: 1234, Bytes: 4096, Kind: uint8(types.CudaEventH2D)}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.dispatch(raw, devmap)
	}
}

// BenchmarkReaderDispatch_SlowResolver 는 실제 cgroup parse 비용을 모사한 30µs 지연 resolver 다.
// 본 수치가 캐시 도입 전 dispatch 의 baseline ns/op 다. 캐시 적용 후 hit 경로 (FakeResolver 수준)
// 와의 차이가 그대로 hot path 가 줄이는 시간이 된다.
func BenchmarkReaderDispatch_SlowResolver(b *testing.B) {
	resolver := slowResolver{
		table: map[uint32]kube.PodIdentity{1234: samplePod("ml", "p", "u")},
		delay: 30 * time.Microsecond,
	}
	r := newReaderForBench(resolver)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1234: "GPU-A"})
	raw := rawEvent{PID: 1234, Bytes: 4096, Kind: uint8(types.CudaEventH2D)}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.dispatch(raw, devmap)
	}
}

// BenchmarkReaderBuildActiveCudaKeys 는 NVML refresh 사이클이 매번 호출하는 PID -> Pod 해상도
// 비용을 측정한다. PID 수에 ResolvePID 호출 비용이 그대로 비례하기 때문에, 캐시 도입 후 lookup
// 으로 대체되면 ns/op 가 PID 수에 거의 비례하지 않는 수준으로 떨어져야 한다.
func BenchmarkReaderBuildActiveCudaKeys(b *testing.B) {
	const pidCount = 64
	table := make(map[uint32]kube.PodIdentity, pidCount)
	pidMap := make(map[uint32]string, pidCount)
	for i := 0; i < pidCount; i++ {
		pid := uint32(1000 + i)
		table[pid] = samplePod("ml", "p"+strconv.Itoa(i), "u"+strconv.Itoa(i))
		pidMap[pid] = "GPU-A"
	}
	resolver := slowResolver{table: table, delay: 30 * time.Microsecond}
	r := New("/unused", "", "node-A", nil, resolver, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.buildActiveCudaKeys(pidMap)
	}
}
