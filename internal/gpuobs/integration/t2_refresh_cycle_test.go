//go:build integration

package integration

import (
	"sync"
	"testing"
	"time"

	"netobs/internal/gpuobs/cuda"
	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// TestT2_RefreshCycleConsistency 는 cuda.Reader 의 refreshOnce 한 사이클 안에서 devicemap /
// podMap / visDev / RetainCudaSeries cleanup / dropped baseline-then-delta 가 모두 일관되게
// 실행되는지 검증한다. fake NVML + fake droppedSource 로 BPF 의존을 제거하고 사이클 단위 정합성에
// 집중한다.
func TestT2_RefreshCycleConsistency(t *testing.T) {
	resetCudaMetrics(t)

	// 단일 GPU + PID 1 으로 시작.
	nv := newFakeNVML([]fakeDevice{{uuid: "GPU-A", index: 0, pids: []uint32{1}}})
	resolver := newStaticResolver(map[uint32]kube.PodIdentity{
		1: samplePod("ml", "p1", "u1"),
	})
	r := cuda.NewIntegrationReader("node-A", nv, resolver, time.Second)
	devSet := nvml.NewDeviceSet(nv)
	defer devSet.Close()

	devmap := cuda.NewDeviceMapForTest()
	dropped := &fakeDroppedSource{}
	var baseline cuda.DroppedBaseline

	// 1 번째 사이클: PID 1 매핑 적재.
	r.RefreshOnceForTest(devSet, devmap, dropped, &baseline)

	if got := devmap.LookupForTest(1); got != "GPU-A" {
		t.Errorf("after cycle 1, devmap[1]=%q want GPU-A", got)
	}
	if !baseline.Initialized() {
		t.Error("dropped baseline not initialized after cycle 1")
	}

	// 2 번째 사이클: PID 2 추가, PID 1 종료.
	nv.setDevices([]fakeDevice{{uuid: "GPU-A", index: 0, pids: []uint32{2}}})
	resolver.set(2, samplePod("ml", "p2", "u2"))
	r.RefreshOnceForTest(devSet, devmap, dropped, &baseline)

	if got := devmap.LookupForTest(1); got != "" {
		t.Errorf("after cycle 2, devmap[1]=%q want empty (PID 1 terminated)", got)
	}
	if got := devmap.LookupForTest(2); got != "GPU-A" {
		t.Errorf("after cycle 2, devmap[2]=%q want GPU-A", got)
	}

	// 3 번째 사이클: dropped 카운터 증가.
	dropped.set(7)
	r.RefreshOnceForTest(devSet, devmap, dropped, &baseline)
	if got := metrics.GetCudaEventsLostForTest("node-A"); got != 7 {
		t.Errorf("events_lost_total=%v want 7 (delta from baseline 0)", got)
	}

	// 4 번째 사이클: dropped 카운터 reset (current < last). 가산 skip + baseline 만 갱신.
	dropped.set(2)
	r.RefreshOnceForTest(devSet, devmap, dropped, &baseline)
	if got := metrics.GetCudaEventsLostForTest("node-A"); got != 7 {
		t.Errorf("events_lost_total=%v want 7 (no delta on reset)", got)
	}

	// 5 번째 사이클: multi-GPU PID 시나리오. PID 3 이 GPU-A 와 GPU-B 양쪽에 등장.
	nv.setDevices([]fakeDevice{
		{uuid: "GPU-A", index: 0, pids: []uint32{3}},
		{uuid: "GPU-B", index: 1, pids: []uint32{3}},
	})
	resolver.set(3, samplePod("ml", "p3", "u3"))
	r.RefreshOnceForTest(devSet, devmap, dropped, &baseline)

	if got := metrics.GetCudaPidMultiGPUCountForTest("node-A"); got != 1 {
		t.Errorf("pid_multi_gpu_count=%v want 1 (PID 3 on 2 GPUs)", got)
	}
}

// ----- 헬퍼 / fake -----

type fakeDevice struct {
	uuid  string
	index uint
	pids  []uint32
}

type fakeNVML struct {
	mu      sync.Mutex
	devices []fakeDevice
}

func newFakeNVML(devs []fakeDevice) *fakeNVML {
	return &fakeNVML{devices: devs}
}

func (f *fakeNVML) setDevices(devs []fakeDevice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = devs
}

func (f *fakeNVML) DeviceCount() (uint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint(len(f.devices)), nil
}

// Device 는 fakeNvmlDevice 를 반환하지만 인스턴스가 부모 fakeNVML 의 현재 device 슬롯을 동적으로
// 다시 lookup 하도록 만든다. nvml.DeviceSet 이 같은 UUID 의 Device 인스턴스를 캐시 후 재사용하기
// 때문에, 인스턴스가 자기 생성 시점의 pids 스냅샷을 그대로 들고 있으면 setDevices 로 갱신한 pids
// 가 후속 RunningProcesses 호출에 반영되지 않는다. 부모 lookup 패턴으로 캐시 정합성을 확보한다.
func (f *fakeNVML) Device(index uint) (nvml.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.devices[index]
	return &fakeNvmlDevice{uuid: d.uuid, parent: f}, nil
}

func (f *fakeNVML) DeviceUUID(index uint) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.devices[index].uuid, nil
}

func (f *fakeNVML) Shutdown() error { return nil }

// currentForUUID 는 부모 fakeNVML 에서 주어진 UUID 의 현재 device 슬롯 정보를 반환한다.
// 슬롯이 사라졌으면 found=false 를 반환해 호출자 (RunningProcesses) 가 빈 결과로 처리한다.
func (f *fakeNVML) currentForUUID(uuid string) (fakeDevice, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		if d.uuid == uuid {
			return d, true
		}
	}
	return fakeDevice{}, false
}

type fakeNvmlDevice struct {
	uuid   string
	parent *fakeNVML
}

func (f *fakeNvmlDevice) Info() (types.GPUDevice, error) {
	if d, ok := f.parent.currentForUUID(f.uuid); ok {
		return types.GPUDevice{UUID: d.uuid, Index: d.index}, nil
	}
	return types.GPUDevice{UUID: f.uuid}, nil
}
func (f *fakeNvmlDevice) Snapshot() (types.GPUSnapshot, error) { return types.GPUSnapshot{}, nil }
func (f *fakeNvmlDevice) RunningProcesses() ([]types.GPUProcess, error) {
	d, ok := f.parent.currentForUUID(f.uuid)
	if !ok {
		return nil, nil
	}
	out := make([]types.GPUProcess, 0, len(d.pids))
	for _, p := range d.pids {
		out = append(out, types.GPUProcess{PID: p})
	}
	return out, nil
}
func (f *fakeNvmlDevice) ProcessUtilization(uint64) ([]types.GPUProcessUtil, error) {
	return nil, nil
}
func (f *fakeNvmlDevice) MigMode() (types.MigMode, error)    { return types.MigModeUnsupported, nil }
func (f *fakeNvmlDevice) MaxMigDeviceCount() (int, error)    { return 0, nil }
func (f *fakeNvmlDevice) MigDevice(int) (nvml.Device, error) { return nil, nil }
func (f *fakeNvmlDevice) IsMigDevice() (bool, error)         { return false, nil }
func (f *fakeNvmlDevice) GpuInstanceId() (uint32, error)     { return 0, nil }
func (f *fakeNvmlDevice) ComputeInstanceId() (uint32, error) { return 0, nil }
func (f *fakeNvmlDevice) Close() error                       { return nil }

type fakeDroppedSource struct {
	mu  sync.Mutex
	val uint64
}

func (f *fakeDroppedSource) Total() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.val
}
func (f *fakeDroppedSource) set(v uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.val = v
}

// staticResolver 는 PID → PodIdentity 의 직접 매핑을 가진 fake resolver.
type staticResolver struct {
	mu    sync.Mutex
	table map[uint32]kube.PodIdentity
}

func newStaticResolver(initial map[uint32]kube.PodIdentity) *staticResolver {
	return &staticResolver{table: initial}
}
func (s *staticResolver) set(pid uint32, id kube.PodIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table[pid] = id
}
func (s *staticResolver) ResolvePID(pid uint32) kube.PodIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.table[pid]
}

func samplePod(ns, name, uid string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     ns,
		PodName:       name,
		PodUID:        uid,
	}
}

// resetCudaMetrics 는 통합 테스트가 동일 process 안에서 누적 metric 시리즈를 공유하지 않도록
// 매 테스트 진입 시점에 cuda 메트릭 상태를 초기화한다.
func resetCudaMetrics(t *testing.T) {
	t.Helper()
	metrics.ResetCudaStateForTest()
}
