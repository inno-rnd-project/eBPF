package cuda

import (
	"testing"

	"netobs/internal/gpuobs/metrics"
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
	r := New("/unused", "node-A", nil, resolver, 0)
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
