package cuda

import (
	"testing"

	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

type fakeResolver struct {
	table map[uint32]kube.PodIdentity
}

func (f fakeResolver) ResolvePID(pid uint32) kube.PodIdentity {
	if id, ok := f.table[pid]; ok {
		return id
	}
	return kube.PodIdentity{}
}

func samplePod(ns, name, uid string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     ns,
		PodName:       name,
		PodUID:        uid,
	}
}

// captureRecorder 는 metrics.RecordCudaEvent 를 가로채 테스트가 dispatch 결과를 직접 검증할 수 있게 한다.
type captureRecorder struct {
	calls []capturedCall
}

type capturedCall struct {
	node   string
	sample metrics.CudaEventSample
}

func (c *captureRecorder) record(node string, sample metrics.CudaEventSample) {
	c.calls = append(c.calls, capturedCall{node: node, sample: sample})
}

func newReaderForDispatch(resolver PodResolver, recorder *captureRecorder) *Reader {
	r := New("/unused", "", "node-A", nil, resolver, 0)
	r.recordEvent = recorder.record
	return r
}

func TestReaderDispatch_ResolvesPodAndGPUFromMaps(t *testing.T) {
	rec := &captureRecorder{}
	resolver := fakeResolver{table: map[uint32]kube.PodIdentity{
		1234: samplePod("ml", "p", "u"),
	}}
	r := newReaderForDispatch(resolver, rec)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1234: "GPU-A"})

	r.dispatch(rawEvent{PID: 1234, Bytes: 4096, Kind: uint8(types.CudaEventH2D)}, devmap)

	if len(rec.calls) != 1 {
		t.Fatalf("captured calls=%d want 1", len(rec.calls))
	}
	got := rec.calls[0]
	if got.node != "node-A" {
		t.Errorf("node=%q want node-A", got.node)
	}
	if got.sample.GPUUUID != "GPU-A" {
		t.Errorf("gpu=%q want GPU-A", got.sample.GPUUUID)
	}
	if got.sample.Kind != types.CudaEventH2D {
		t.Errorf("kind=%v want H2D", got.sample.Kind)
	}
	if got.sample.Bytes != 4096 {
		t.Errorf("bytes=%d want 4096", got.sample.Bytes)
	}
	if !got.sample.ID.IsPod() || got.sample.ID.PodName != "p" {
		t.Errorf("identity=%+v want pod p", got.sample.ID)
	}
}

func TestReaderDispatch_UnknownGPUUUIDPassedAsEmpty(t *testing.T) {
	// PID 가 어떤 GPU 에도 등록되지 않았으면 devmap.lookup 이 빈 문자열을 반환하고,
	// 이는 dispatch 가 그대로 전달해 metrics 계층이 "unknown" 으로 폴백하도록 한다 (metrics 측에서 검증됨).
	rec := &captureRecorder{}
	resolver := fakeResolver{table: map[uint32]kube.PodIdentity{
		1234: samplePod("ml", "p", "u"),
	}}
	r := newReaderForDispatch(resolver, rec)
	devmap := newDeviceMap()
	// PID 1234 등록 없음 → lookup 시 빈 문자열

	r.dispatch(rawEvent{PID: 1234, Kind: uint8(types.CudaEventKernelLaunch)}, devmap)

	if len(rec.calls) != 1 {
		t.Fatalf("captured calls=%d want 1", len(rec.calls))
	}
	if got := rec.calls[0].sample.GPUUUID; got != "" {
		t.Errorf("gpu uuid=%q want empty (will be 'unknown' in metrics layer)", got)
	}
}

func TestReaderDispatch_NilResolverPassesEmptyIdentity(t *testing.T) {
	// resolver 가 nil 이면 ID 는 zero value (IsPod=false). metrics 측이 발행을 skip 한다 (metrics 측에서 검증됨).
	rec := &captureRecorder{}
	r := newReaderForDispatch(nil, rec)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1234: "GPU-A"})

	r.dispatch(rawEvent{PID: 1234, Kind: uint8(types.CudaEventKernelLaunch)}, devmap)

	if len(rec.calls) != 1 {
		t.Fatalf("captured calls=%d want 1", len(rec.calls))
	}
	if rec.calls[0].sample.ID.IsPod() {
		t.Errorf("identity must not be pod when resolver=nil; got %+v", rec.calls[0].sample.ID)
	}
}

func TestReaderBuildActiveCudaKeys_NilResolverReturnsEmpty(t *testing.T) {
	// resolver 가 nil 이면 RetainCudaSeries 호출 시 모든 시리즈가 제거되도록 빈 셋을 반환해야 한다.
	r := New("/unused", "", "node-A", nil, nil, 0)

	keys := r.buildActiveCudaKeys(map[uint32]string{1: "G"})

	if len(keys) != 0 {
		t.Errorf("nil resolver must yield empty active set; got %d keys", len(keys))
	}
}

func TestReaderBuildActiveCudaKeys_NonPodIdentitySkipped(t *testing.T) {
	// Pod 으로 해석되지 않은 PID 는 active 셋에 들어가지 않아 RetainCudaSeries 가 자연 cleanup 한다.
	r := New("/unused", "", "node-A", nil, fakeResolver{table: map[uint32]kube.PodIdentity{
		1: {IdentityClass: kube.IdentityClassNode, NodeName: "n1"},
		2: {IdentityClass: kube.IdentityClassExternal},
	}}, 0)

	keys := r.buildActiveCudaKeys(map[uint32]string{1: "G", 2: "G"})

	if len(keys) != 0 {
		t.Errorf("non-pod identities must be skipped; got %d keys", len(keys))
	}
}

func TestReaderBuildActiveCudaKeys_KeyFormatMatchesRecordCudaEvent(t *testing.T) {
	// buildActiveCudaKeys 가 만든 키 형식이 RecordCudaEvent 가 사용하는 metrics.CudaActiveKey 와
	// 정확히 동일해야 한다. PodName/UID 폴백 분기까지 포함해 검증한다.
	id := samplePod("ml", "p", "u")
	r := New("/unused", "", "node-A", nil, fakeResolver{table: map[uint32]kube.PodIdentity{1: id}}, 0)

	keys := r.buildActiveCudaKeys(map[uint32]string{1: "G"})

	want := metrics.CudaActiveKey("node-A", "ml", "p", "u", "G")
	if _, ok := keys[want]; !ok {
		t.Errorf("expected key %q in active set; got %v", want, keys)
	}

	// Pod 으로 분류되었지만 PodName/PodUID 가 비어 있는 케이스: metrics 의 PodNameOrUnknown / PodUIDOrUnknown
	// 폴백과 동일한 "unknown" 으로 키가 만들어져야 한다.
	idEmpty := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ml"}
	r2 := New("/unused", "", "node-A", nil, fakeResolver{table: map[uint32]kube.PodIdentity{2: idEmpty}}, 0)
	keys2 := r2.buildActiveCudaKeys(map[uint32]string{2: "G"})
	want2 := metrics.CudaActiveKey("node-A", "ml", "unknown", "unknown", "G")
	if _, ok := keys2[want2]; !ok {
		t.Errorf("empty pod name/uid must fallback to 'unknown'; got %v", keys2)
	}
}

func TestReaderBuildActiveCudaKeys_DuplicatePidDeduped(t *testing.T) {
	// 동일 PID 가 두 번 등장할 일은 정상 경로엔 없지만 (map key 자체가 unique), 동일 (Pod, GPU)
	// 매핑이 만들어내는 키는 map 의 자연 dedupe 로 1 슬롯이 되어야 한다.
	id := samplePod("ml", "p", "u")
	r := New("/unused", "", "node-A", nil, fakeResolver{table: map[uint32]kube.PodIdentity{1: id}}, 0)

	keys := r.buildActiveCudaKeys(map[uint32]string{1: "G"})
	if len(keys) != 1 {
		t.Errorf("single pid → single key; got %d", len(keys))
	}
}

// fakeNvmlDevice 는 cuda 패키지가 nvml.Device 인터페이스를 통해 다루는 device 를 단위 테스트용으로
// 모사한다. Info / RunningProcesses 만 collectPidToUUID 가 호출하므로 나머지 메서드는 zero return 으로 둔다.
type fakeNvmlDevice struct {
	uuid string
	pids []uint32
}

func (f *fakeNvmlDevice) Info() (types.GPUDevice, error) {
	return types.GPUDevice{UUID: f.uuid}, nil
}
func (f *fakeNvmlDevice) Snapshot() (types.GPUSnapshot, error) {
	return types.GPUSnapshot{}, nil
}
func (f *fakeNvmlDevice) RunningProcesses() ([]types.GPUProcess, error) {
	out := make([]types.GPUProcess, 0, len(f.pids))
	for _, p := range f.pids {
		out = append(out, types.GPUProcess{PID: p})
	}
	return out, nil
}
func (f *fakeNvmlDevice) Close() error { return nil }

func TestReaderCollectPidToUUID_LastWinsAndCountsMultiGPU(t *testing.T) {
	devA := &fakeNvmlDevice{uuid: "GPU-A", pids: []uint32{1, 2}}
	devB := &fakeNvmlDevice{uuid: "GPU-B", pids: []uint32{2, 3}}
	r := New("/unused", "", "node-A", nil, nil, 0)

	fresh, multi := r.collectPidToUUID([]nvml.Device{devA, devB})

	if got := fresh[1]; got != "GPU-A" {
		t.Errorf("pid=1 uuid=%q want GPU-A", got)
	}
	if got := fresh[2]; got != "GPU-B" {
		t.Errorf("pid=2 uuid=%q want GPU-B (last-wins)", got)
	}
	if got := fresh[3]; got != "GPU-B" {
		t.Errorf("pid=3 uuid=%q want GPU-B", got)
	}
	if multi != 1 {
		t.Errorf("multiGPUCount=%d want 1 (only pid 2 is on >1 GPU)", multi)
	}
}

func TestReaderCollectPidToUUID_AllSingleGPUYieldsZeroCount(t *testing.T) {
	// DDP 류 GPU 당 1 프로세스 패턴: 어떤 PID 도 둘 이상 GPU 에 등장하지 않는다.
	devA := &fakeNvmlDevice{uuid: "GPU-A", pids: []uint32{1, 2}}
	devB := &fakeNvmlDevice{uuid: "GPU-B", pids: []uint32{3, 4}}
	r := New("/unused", "", "node-A", nil, nil, 0)

	_, multi := r.collectPidToUUID([]nvml.Device{devA, devB})

	if multi != 0 {
		t.Errorf("multiGPUCount=%d want 0", multi)
	}
}

func TestReaderResolvePidToPod_NilResolverReturnsEmpty(t *testing.T) {
	r := New("/unused", "", "node-A", nil, nil, 0)
	got := r.resolvePidToPod(map[uint32]string{1: "G"})
	if len(got) != 0 {
		t.Errorf("nil resolver must yield empty map; got %d entries", len(got))
	}
}

func TestReaderResolvePidToPod_CallsResolverForEachPid(t *testing.T) {
	resolver := &countingResolver{
		PodResolver: fakeResolver{table: map[uint32]kube.PodIdentity{
			1: samplePod("ml", "p1", "u1"),
			2: samplePod("ml", "p2", "u2"),
		}},
	}
	r := New("/unused", "", "node-A", nil, resolver, 0)

	got := r.resolvePidToPod(map[uint32]string{1: "G", 2: "G", 3: "G"})

	if resolver.count != 3 {
		t.Errorf("ResolvePID count=%d want 3", resolver.count)
	}
	if len(got) != 3 {
		t.Errorf("result size=%d want 3", len(got))
	}
	if got[1].PodName != "p1" {
		t.Errorf("got[1].PodName=%q want p1", got[1].PodName)
	}
	if got[3].IsPod() {
		t.Errorf("got[3] expected zero (non-pod), got %+v", got[3])
	}
}

// countingResolver 는 ResolvePID 호출 횟수를 추적해 캐시 히트율을 검증한다.
type countingResolver struct {
	PodResolver
	count int
}

func (c *countingResolver) ResolvePID(pid uint32) kube.PodIdentity {
	c.count++
	return c.PodResolver.ResolvePID(pid)
}

// TestReaderDispatch_CacheMissThenHit 는 dispatch 가 첫 이벤트에서 ResolvePID 를 호출해
// 캐시에 적재하고, 같은 PID 의 후속 이벤트가 캐시 hit 경로로 들어가 ResolvePID 가 한 번만
// 호출되는지 검증한다. 이 동작이 PID→Pod 캐시의 본질이다.
func TestReaderDispatch_CacheMissThenHit(t *testing.T) {
	rec := &captureRecorder{}
	resolver := &countingResolver{
		PodResolver: fakeResolver{table: map[uint32]kube.PodIdentity{
			1234: samplePod("ml", "p", "u"),
		}},
	}
	r := newReaderForDispatch(resolver, rec)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1234: "GPU-A"})

	for i := 0; i < 5; i++ {
		r.dispatch(rawEvent{PID: 1234, Kind: uint8(types.CudaEventKernelLaunch)}, devmap)
	}

	if got := resolver.count; got != 1 {
		t.Errorf("ResolvePID called %d times across 5 dispatches; want 1 (cache hit)", got)
	}
	if len(rec.calls) != 5 {
		t.Errorf("captured calls=%d want 5", len(rec.calls))
	}
}

// TestReaderDispatch_NegativeResultCached 는 비-Pod PID 에 대한 zero PodIdentity 도 캐시되어
// 후속 이벤트에서 ResolvePID 가 다시 호출되지 않는지 검증한다. 호스트 프로세스가 이벤트를
// 발생시키는 환경에서 동일 PID 의 cgroup parse 가 반복되지 않게 하는 핵심 동작이다.
func TestReaderDispatch_NegativeResultCached(t *testing.T) {
	rec := &captureRecorder{}
	resolver := &countingResolver{PodResolver: fakeResolver{table: map[uint32]kube.PodIdentity{}}}
	r := newReaderForDispatch(resolver, rec)
	devmap := newDeviceMap()

	for i := 0; i < 3; i++ {
		r.dispatch(rawEvent{PID: 9999, Kind: uint8(types.CudaEventKernelLaunch)}, devmap)
	}

	if got := resolver.count; got != 1 {
		t.Errorf("ResolvePID for non-pod PID called %d times; want 1 (negative cache)", got)
	}
}

func TestReaderDispatch_KindPassThrough(t *testing.T) {
	// rawEvent.Kind(uint8) 가 types.CudaEventKind 로 그대로 전달되어야 한다.
	rec := &captureRecorder{}
	resolver := fakeResolver{table: map[uint32]kube.PodIdentity{1: samplePod("ml", "p", "u")}}
	r := newReaderForDispatch(resolver, rec)
	devmap := newDeviceMap()
	devmap.replace(map[uint32]string{1: "G"})

	cases := []types.CudaEventKind{
		types.CudaEventKernelLaunch,
		types.CudaEventH2D,
		types.CudaEventD2H,
	}
	for _, k := range cases {
		r.dispatch(rawEvent{PID: 1, Kind: uint8(k)}, devmap)
	}

	if len(rec.calls) != 3 {
		t.Fatalf("calls=%d want 3", len(rec.calls))
	}
	for i, k := range cases {
		if rec.calls[i].sample.Kind != k {
			t.Errorf("kind[%d]=%v want %v", i, rec.calls[i].sample.Kind, k)
		}
	}
}
