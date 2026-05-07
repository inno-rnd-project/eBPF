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
// I/O 비용을 마이크로벤치에서 재현하기 위한 blocking-syscall 모델 fake 다. 기본 30µs sleep
// 으로 호출하지만, Linux time.Sleep 의 OS 스케줄러 그라뉼러리티 (HZ 설정 / hrtimer 정밀도)
// 영향으로 실측 ns/op 는 약 1 ms 수준까지 늘어난다. 이는 dev 노드의 실제 cgroup parse 비용
// (~30 µs) 보다 크지만, 본 벤치의 본질은 캐시 hit (RWMutex.RLock 기반) 와 캐시 miss
// (blocking-syscall 시뮬레이션) 의 상대 ns/op 비율이라 절대값 자체에는 의미를 두지 않는다.
// busy-wait 으로 정확히 30 µs 를 만들 수도 있지만, 그 경우 CPU 점유 모델로 바뀌어 실제 cgroup
// parse 의 I/O wait 특성을 모사하지 못해 채택하지 않는다.
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

// BenchmarkReaderDispatch_CacheMiss_Slow 는 매 iteration 마다 다른 PID 를 사용해 캐시를 항상
// miss 시킨다. 본 수치는 캐시가 무효화된 최악 경로 (예: PID wraparound, 또는 모든 이벤트가
// 신규 PID 인 가상의 worst case) 의 ns/op 를 보여준다. 캐시 hit 경로 벤치와 비교했을 때 캐시
// 도입이 흡수하는 시간이 그대로 ns/op 차이로 드러난다.
//
// tableSize 는 4096 으로 한정해 fake resolver 의 메모리 footprint 를 작게 유지한다. 30 µs sleep
// 기반 resolver 의 실측 비용이 ~1 ms 수준이라 benchtime 300 ms 에서 b.N 은 수백 단위 정도이며,
// 4096 PID cycle 만으로도 모든 iter 의 PID 가 unique 함이 보장된다.
func BenchmarkReaderDispatch_CacheMiss_Slow(b *testing.B) {
	const tableSize = 1 << 12
	table := make(map[uint32]kube.PodIdentity, tableSize)
	for i := uint32(0); i < tableSize; i++ {
		table[i] = samplePod("ml", "p", "u")
	}
	resolver := slowResolver{table: table, delay: 30 * time.Microsecond}
	r := newReaderForBench(resolver)
	devmap := newDeviceMap()
	raw := rawEvent{Bytes: 4096, Kind: uint8(types.CudaEventH2D)}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		raw.PID = uint32(i) & (tableSize - 1)
		r.dispatch(raw, devmap)
	}
}

// BenchmarkReaderDispatch_CacheHit 는 podMap 에 PID 가 이미 적재된 상태의 hot path 를 측정한다.
// SlowResolver baseline 과 비교했을 때 캐시 도입이 절감하는 시간이 ns/op 차이로 직접 드러난다.
func BenchmarkReaderDispatch_CacheHit(b *testing.B) {
	resolver := slowResolver{
		table: map[uint32]kube.PodIdentity{1234: samplePod("ml", "p", "u")},
		delay: 30 * time.Microsecond,
	}
	r := newReaderForBench(resolver)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1234: "GPU-A"})
	// 미리 캐시를 채워 모든 dispatch 가 hit 경로로 들어가게 만든다.
	r.pods.store(1234, samplePod("ml", "p", "u"))
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
