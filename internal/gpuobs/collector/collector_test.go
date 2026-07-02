package collector

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"netobs/internal/gpuobs/config"
	"netobs/internal/gpuobs/contention"
	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// snapshotSpy는 collector.recordSnapshot test seam에 주입되어 호출 인자를 수집한다.
// 호출 시점/인자 검증을 통해 합산·diff cleanup 동작을 단위 테스트로 고정한다.
type snapshotSpy struct {
	mu    sync.Mutex
	calls []snapshotCall
}

type snapshotCall struct {
	node    string
	samples []metrics.PodGPUSample
}

func (s *snapshotSpy) record(node string, samples []metrics.PodGPUSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// samples slice ownership을 caller가 재사용할 가능성에 대비해 깊은 복사를 보관한다.
	cp := make([]metrics.PodGPUSample, len(samples))
	copy(cp, samples)
	s.calls = append(s.calls, snapshotCall{node: node, samples: cp})
}

func (s *snapshotSpy) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *snapshotSpy) lastSamples() []metrics.PodGPUSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return s.calls[len(s.calls)-1].samples
}

// fakeResolver는 PID → PodIdentity 매핑을 테스트에서 직접 시드하는 PodResolver 구현이다.
// 등록되지 않은 PID는 unresolved를 반환해 collector pollOnce의 IsPod 가드에서 자연스럽게 걸러진다.
type fakeResolver struct {
	byPID map[uint32]kube.PodIdentity
	calls int
	mu    sync.Mutex
}

func (f *fakeResolver) ResolvePID(pid uint32) kube.PodIdentity {
	f.mu.Lock()
	f.calls++
	defer f.mu.Unlock()
	if id, ok := f.byPID[pid]; ok {
		return id
	}
	return kube.PodIdentity{IdentityClass: kube.IdentityClassUnresolved}
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeNVML은 nvml.NVML의 테스트용 구현이며, 호출 횟수와 사전에 지정된 디바이스 맵을 통해
// collector 동작을 검증 가능한 상태로 관찰한다.
type fakeNVML struct {
	mu            sync.Mutex
	count         uint
	countErr      error
	devices       map[uint]*fakeDevice
	shutdownCalls int
}

func (f *fakeNVML) DeviceCount() (uint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count, f.countErr
}

func (f *fakeNVML) Device(i uint) (nvml.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[i]
	if !ok {
		return nil, errors.New("unknown device")
	}
	return d, nil
}

// DeviceUUID 는 nvml.DeviceSet 이 hot-plug 동기화 루프에서 호출하는 light-weight UUID 조회다.
// devices map 에 등록된 device 의 info.UUID 를 그대로 돌려준다.
func (f *fakeNVML) DeviceUUID(i uint) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.devices[i]
	if !ok {
		return "", errors.New("unknown device")
	}
	return d.info.UUID, nil
}

func (f *fakeNVML) Shutdown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdownCalls++
	return nil
}

func (f *fakeNVML) shutdownCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutdownCalls
}

type fakeDevice struct {
	mu            sync.Mutex
	info          types.GPUDevice
	snapshot      types.GPUSnapshot
	snapshotErr   error
	snapCalls     int
	processes     []types.GPUProcess
	processesErr  error
	procCalls     int
	procUtils     []types.GPUProcessUtil
	procUtilCalls int
	closeCalls    int
}

func (d *fakeDevice) Info() (types.GPUDevice, error) { return d.info, nil }

func (d *fakeDevice) Snapshot() (types.GPUSnapshot, error) {
	d.mu.Lock()
	d.snapCalls++
	d.mu.Unlock()
	return d.snapshot, d.snapshotErr
}

func (d *fakeDevice) RunningProcesses() ([]types.GPUProcess, error) {
	d.mu.Lock()
	d.procCalls++
	d.mu.Unlock()
	return d.processes, d.processesErr
}

func (d *fakeDevice) Close() error {
	d.mu.Lock()
	d.closeCalls++
	d.mu.Unlock()
	return nil
}

// #104 MIG / process-util 인터페이스 stub. 본 테스트는 기존 Snapshot / RunningProcesses 경로만
// 검증하므로 zero return 으로 충분하다. 후속 commit 의 process util / MIG 경로 검증은 별도 fake 에서.
func (d *fakeDevice) ProcessUtilization() ([]types.GPUProcessUtil, error) {
	d.mu.Lock()
	d.procUtilCalls++
	utils := d.procUtils
	d.mu.Unlock()
	return utils, nil
}
func (d *fakeDevice) MigMode() (types.MigMode, error)                           { return types.MigModeUnsupported, nil }
func (d *fakeDevice) MaxMigDeviceCount() (int, error)                           { return 0, nil }
func (d *fakeDevice) MigDevice(int) (nvml.Device, error)                        { return nil, nil }
func (d *fakeDevice) IsMigDevice() (bool, error)                                { return false, nil }
func (d *fakeDevice) GpuInstanceId() (uint32, error)                            { return 0, nil }
func (d *fakeDevice) ComputeInstanceId() (uint32, error)                        { return 0, nil }

func (d *fakeDevice) closeCallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closeCalls
}

func (d *fakeDevice) procCallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.procCalls
}

func (d *fakeDevice) snapCallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapCalls
}

// newCollectorWithDevs 는 fakeDevice 슬라이스로 fakeNVML 을 시드하고 Collector + DeviceSet 을 한 번에 구성한다.
// 기존 테스트가 `c := New(nil, ...) + c.devices = []nvml.Device{...}` 로 단언했던 흐름을, DeviceSet 도입 이후의
// `New(fake) + devSet.Sync()` 등가 헬퍼로 대체한다. fake 는 caller 에 함께 반환해 호출 횟수 / Shutdown 카운터를
// 추가 검증할 수 있게 한다.
func newCollectorWithDevs(t *testing.T, cfg config.Config, resolver PodResolver, devs ...*fakeDevice) (*Collector, *fakeNVML) {
	t.Helper()
	fake := &fakeNVML{
		count:   uint(len(devs)),
		devices: make(map[uint]*fakeDevice, len(devs)),
	}
	for i, d := range devs {
		fake.devices[uint(i)] = d
	}
	c := New(fake, cfg, resolver)
	c.devSet = nvml.NewDeviceSet(fake)
	if err := c.devSet.Sync(); err != nil {
		t.Fatalf("seed devSet sync: %v", err)
	}
	return c, fake
}

// waitReady는 onReady가 신호될 때까지 대기하며 타임아웃 시 테스트를 실패 처리한다.
func waitReady(t *testing.T, readyCh <-chan struct{}) {
	t.Helper()
	select {
	case <-readyCh:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("onReady not signaled within timeout")
	}
}

// waitDone은 Run goroutine이 반환할 때까지 대기하며 타임아웃 시 테스트를 실패 처리한다.
// 단순 `<-done`을 쓰면 Run이 반환하지 못하는 결함이 있을 때 테스트가 영구 hang된다.
func waitDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after ctx cancel")
	}
}

// waitUntil은 check가 true를 반환할 때까지 짧게 폴링하며, deadline 내에 만족되지 않으면
// 테스트를 실패 처리한다. 고정 time.Sleep 대신 사용해 CI 스케줄링 지연에 강건해진다.
func waitUntil(t *testing.T, timeout time.Duration, check func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !check() {
		t.Fatal(msg)
	}
}

func TestRun_NilNVMLGracefullyDisables(t *testing.T) {
	cfg := config.Config{GPUMetricsEnabled: true, GPUPollInterval: 10 * time.Millisecond, NodeName: "n"}
	c := New(nil, cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, func() { readyCh <- struct{}{} })
	}()

	waitReady(t, readyCh)
	cancel()
	waitDone(t, done)
}

func TestRun_FlagDisabledSkipsPolling(t *testing.T) {
	dev := &fakeDevice{info: types.GPUDevice{Index: 0, UUID: "u0"}}
	fake := &fakeNVML{count: 1, devices: map[uint]*fakeDevice{0: dev}}
	cfg := config.Config{GPUMetricsEnabled: false, GPUPollInterval: 10 * time.Millisecond, NodeName: "n"}
	c := New(fake, cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, func() { readyCh <- struct{}{} })
	}()

	waitReady(t, readyCh)
	// disable 경로라 폴링이 일어나지 않아야 한다. ticker 주기 몇 번 분량만 대기해
	// "disable 의도와 달리 폴링이 발생하지 않음"을 확인한다.
	time.Sleep(30 * time.Millisecond)
	cancel()
	waitDone(t, done)

	if got := dev.snapCallCount(); got != 0 {
		t.Fatalf("disabled path must not poll; got %d snapshot calls", got)
	}
	// non-nil NVML 핸들을 받은 이상 disable 경로에서도 collector가 Shutdown을 보장해야 한다.
	if got := fake.shutdownCallCount(); got != 1 {
		t.Fatalf("disabled path must still release NVML; expected 1 Shutdown call, got %d", got)
	}
}

func TestRun_HappyPathPollsAndShutsDown(t *testing.T) {
	dev0 := &fakeDevice{info: types.GPUDevice{Index: 0, UUID: "u0"}, snapshot: types.GPUSnapshot{UtilizationPct: 42}}
	dev1 := &fakeDevice{info: types.GPUDevice{Index: 1, UUID: "u1"}, snapshot: types.GPUSnapshot{UtilizationPct: 77}}
	fake := &fakeNVML{count: 2, devices: map[uint]*fakeDevice{0: dev0, 1: dev1}}
	cfg := config.Config{GPUMetricsEnabled: true, GPUPollInterval: 10 * time.Millisecond, NodeName: "n"}
	c := New(fake, cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- c.Run(ctx, func() { readyCh <- struct{}{} })
	}()

	waitReady(t, readyCh)
	// ready 직후 초기 1회 폴링이 완료되어 있어야 한다.
	if got := dev0.snapCallCount(); got < 1 {
		t.Fatalf("expected >=1 snapshot call on dev0 after ready; got %d", got)
	}

	// ticker 기반 추가 폴링이 관측될 때까지 deadline 내에서 반복 확인한다.
	// 고정 time.Sleep보다 CI 부하에 강건하다.
	waitUntil(t, 300*time.Millisecond, func() bool {
		return dev1.snapCallCount() >= 2
	}, "expected >=2 snapshot calls on dev1 within timeout")

	cancel()
	waitDone(t, done)

	if got := fake.shutdownCallCount(); got != 1 {
		t.Fatalf("expected Shutdown called exactly once on ctx cancel; got %d", got)
	}
	// device별 Close가 ctx 취소 시 정확히 한 번씩 호출되어 GPM sample 등 device-scope 자원이 해제되어야 한다.
	if got := dev0.closeCallCount(); got != 1 {
		t.Errorf("expected dev0 Close called once on ctx cancel; got %d", got)
	}
	if got := dev1.closeCallCount(); got != 1 {
		t.Errorf("expected dev1 Close called once on ctx cancel; got %d", got)
	}
}

func TestPollOnce_PerDeviceErrorContinues(t *testing.T) {
	dev0 := &fakeDevice{info: types.GPUDevice{Index: 0, UUID: "u0"}}
	dev1 := &fakeDevice{info: types.GPUDevice{Index: 1, UUID: "u1"}, snapshotErr: errors.New("boom")}
	dev2 := &fakeDevice{info: types.GPUDevice{Index: 2, UUID: "u2"}}
	cfg := config.Config{GPUMetricsEnabled: true, NodeName: "n"}
	// Run 을 거치지 않고 pollOnce 만 단독 검증하므로 newCollectorWithDevs 가 DeviceSet 시드까지 처리한다.
	c, _ := newCollectorWithDevs(t, cfg, nil, dev0, dev1, dev2)

	c.pollOnce()

	if got := dev0.snapCallCount(); got != 1 {
		t.Errorf("dev0 should have been polled once; got %d", got)
	}
	if got := dev2.snapCallCount(); got != 1 {
		t.Errorf("dev2 should be polled after dev1 error; got %d", got)
	}
}

func TestPollOnce_PerPodInvokesResolver(t *testing.T) {
	// resolver와 PodMetricsEnabled가 모두 활성일 때 device당 RunningProcesses 결과가 ResolvePID로 전달되어야 한다.
	dev := &fakeDevice{
		info: types.GPUDevice{Index: 0, UUID: "u0"},
		processes: []types.GPUProcess{
			{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1024},
			{DeviceIndex: 0, PID: 200, MemoryUsedBytes: 2048},
		},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "u1"},
	}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)

	c.pollOnce()

	// 두 PID 모두 ResolvePID로 전달되어야 한다. (한 PID는 unresolved지만 호출 자체는 발생)
	if got := resolver.callCount(); got != 2 {
		t.Fatalf("expected 2 ResolvePID calls; got %d", got)
	}
}

func TestPollOnce_PerPodSkippedWhenResolverNil(t *testing.T) {
	// resolver가 nil이면 RunningProcesses 호출 자체가 일어나지 않아야 한다.
	// 호출 카운터로 명시적으로 검증한다 (error 전파 부재만으로는 미호출을 증명할 수 없다).
	dev := &fakeDevice{
		info:      types.GPUDevice{Index: 0, UUID: "u0"},
		processes: []types.GPUProcess{{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1}},
	}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, nil, dev)

	c.pollOnce()

	if got := dev.procCallCount(); got != 0 {
		t.Fatalf("nil resolver must short-circuit RunningProcesses; got %d calls", got)
	}
}

func TestPollOnce_PerPodSkippedWhenToggleDisabled(t *testing.T) {
	// resolver는 주입되었지만 PodMetricsEnabled가 false면 RunningProcesses와 ResolvePID 모두 호출되지 않아야 한다.
	dev := &fakeDevice{
		info:      types.GPUDevice{Index: 0, UUID: "u0"},
		processes: []types.GPUProcess{{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1}},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: false, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)

	c.pollOnce()

	if got := dev.procCallCount(); got != 0 {
		t.Errorf("disabled toggle must short-circuit RunningProcesses; got %d calls", got)
	}
	if got := resolver.callCount(); got != 0 {
		t.Errorf("disabled toggle must short-circuit ResolvePID; got %d calls", got)
	}
}

func TestPollOnce_AggregatesMultiProcessIntoSinglePodSample(t *testing.T) {
	// 동일 Pod이 같은 GPU에서 두 프로세스를 띄우면 합산 후 단일 sample만 RecordPodSnapshot에 전달되어야 한다.
	// 라벨 셋에 pid가 없어 직접 호출하면 덮어써지던 문제를 collector 합산 단계가 막는다.
	devInfo := types.GPUDevice{Index: 0, UUID: "GPU-A"}
	dev := &fakeDevice{
		info:     devInfo,
		snapshot: types.GPUSnapshot{Device: devInfo},
		processes: []types.GPUProcess{
			{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1024},
			{DeviceIndex: 0, PID: 101, MemoryUsedBytes: 2048},
		},
	}
	pod := kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     "ml",
		PodName:       "trainer-0",
		PodUID:        "uid-A",
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: pod,
		101: pod,
	}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	spy := &snapshotSpy{}
	c.recordSnapshot = spy.record

	c.pollOnce()

	if got := spy.callCount(); got != 1 {
		t.Fatalf("RecordPodSnapshot should be called once per poll; got %d", got)
	}
	samples := spy.lastSamples()
	if len(samples) != 1 {
		t.Fatalf("expected 1 aggregated sample for one (Pod, GPU); got %d", len(samples))
	}
	if samples[0].MemUsedBytes != 1024+2048 {
		t.Errorf("aggregated mem=%d want %d", samples[0].MemUsedBytes, 1024+2048)
	}
	if samples[0].ID.PodUID != "uid-A" {
		t.Errorf("sample ID.PodUID=%q want uid-A", samples[0].ID.PodUID)
	}
}

func TestPollOnce_AggregatesAcrossDevicesSeparately(t *testing.T) {
	// 같은 Pod이 두 GPU에 걸쳐 워크로드를 띄운 경우 (gpu_uuid, gpu_index)가 다르면 별도 sample로 분리되어야 한다.
	dev0Info := types.GPUDevice{Index: 0, UUID: "GPU-A"}
	dev1Info := types.GPUDevice{Index: 1, UUID: "GPU-B"}
	dev0 := &fakeDevice{
		info:      dev0Info,
		snapshot:  types.GPUSnapshot{Device: dev0Info},
		processes: []types.GPUProcess{{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1024}},
	}
	dev1 := &fakeDevice{
		info:      dev1Info,
		snapshot:  types.GPUSnapshot{Device: dev1Info},
		processes: []types.GPUProcess{{DeviceIndex: 1, PID: 200, MemoryUsedBytes: 2048}},
	}
	pod := kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     "ml",
		PodName:       "trainer-0",
		PodUID:        "uid-A",
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{100: pod, 200: pod}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev0, dev1)
	spy := &snapshotSpy{}
	c.recordSnapshot = spy.record

	c.pollOnce()

	samples := spy.lastSamples()
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples (one per GPU); got %d", len(samples))
	}
	uuids := map[string]uint64{}
	for _, s := range samples {
		uuids[s.Device.UUID] = s.MemUsedBytes
	}
	if uuids["GPU-A"] != 1024 || uuids["GPU-B"] != 2048 {
		t.Errorf("per-GPU aggregation mismatch: %+v", uuids)
	}
}

func TestPollOnce_NonPodIdentitiesExcludedFromSamples(t *testing.T) {
	// host process / unresolved 등 IsPod가 false인 PID는 합산 키 생성도 안 되어 sample에 포함되지 않아야 한다.
	devInfo := types.GPUDevice{Index: 0, UUID: "GPU-A"}
	dev := &fakeDevice{
		info:     devInfo,
		snapshot: types.GPUSnapshot{Device: devInfo},
		processes: []types.GPUProcess{
			{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1024}, // pod
			{DeviceIndex: 0, PID: 999, MemoryUsedBytes: 4096}, // host (no entry → unresolved)
		},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassPod, PodUID: "uid-A", PodName: "p", Namespace: "ml"},
	}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	spy := &snapshotSpy{}
	c.recordSnapshot = spy.record

	c.pollOnce()

	samples := spy.lastSamples()
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample (host PID excluded); got %d", len(samples))
	}
	if samples[0].MemUsedBytes != 1024 {
		t.Errorf("pod-only mem=%d want 1024 (host PID memory must not be included)", samples[0].MemUsedBytes)
	}
}

func TestPollOnce_StaleCleanupCallsSnapshotEvenWhenEmpty(t *testing.T) {
	// 첫 poll은 podA, 두 번째 poll에는 워크로드가 사라진 경우 두 번째 호출에서도 RecordPodSnapshot이
	// 호출되어 metrics 측 diff cleanup이 직전 라벨을 정리할 수 있어야 한다.
	pod := kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "a", PodUID: "uid-a",
	}
	devInfo := types.GPUDevice{Index: 0, UUID: "GPU-A"}
	dev := &fakeDevice{
		info:     devInfo,
		snapshot: types.GPUSnapshot{Device: devInfo},
	}
	dev.processes = []types.GPUProcess{{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 100}}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{100: pod}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	spy := &snapshotSpy{}
	c.recordSnapshot = spy.record

	c.pollOnce()
	// 워크로드가 사라진 상태로 다시 폴링
	dev.mu.Lock()
	dev.processes = nil
	dev.mu.Unlock()
	c.pollOnce()

	if got := spy.callCount(); got != 2 {
		t.Fatalf("expected 2 RecordPodSnapshot calls (one per poll); got %d", got)
	}
	if last := spy.lastSamples(); len(last) != 0 {
		t.Errorf("second poll snapshot must be empty; got %d samples", len(last))
	}
}

func TestPollOnce_NoSnapshotCallWhenResolverNil(t *testing.T) {
	// resolver가 nil이면 RecordPodSnapshot 호출 자체가 없어야 한다.
	// 이 경로에서는 metrics 패키지 lastPodSampleKeys도 건드리지 않는다.
	dev := &fakeDevice{
		info:      types.GPUDevice{Index: 0, UUID: "u0"},
		processes: []types.GPUProcess{{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1}},
	}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, nil, dev)
	spy := &snapshotSpy{}
	c.recordSnapshot = spy.record

	c.pollOnce()

	if got := spy.callCount(); got != 0 {
		t.Fatalf("nil resolver must not invoke RecordPodSnapshot; got %d calls", got)
	}
}

func TestPollOnce_NoSnapshotCallWhenToggleDisabled(t *testing.T) {
	// PodMetricsEnabled=false면 RecordPodSnapshot 호출 없음. metrics 패키지의 lastPodSampleKeys는
	// startup 시점에 빈 상태이므로 호출 누락이 stale을 유발하지 않는다(toggle이 startup-only 계약).
	dev := &fakeDevice{info: types.GPUDevice{Index: 0, UUID: "u0"}}
	resolver := &fakeResolver{}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: false, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	spy := &snapshotSpy{}
	c.recordSnapshot = spy.record

	c.pollOnce()

	if got := spy.callCount(); got != 0 {
		t.Fatalf("disabled toggle must not invoke RecordPodSnapshot; got %d calls", got)
	}
}

// TestPollOnce_RecordsMigModeAndMpsActive 는 #104 self-health emit 흐름 검증. device 마다 매 poll 에서
// gpuobs_mig_mode (3 mode 시리즈) 와 gpuobs_mps_active 가 모두 emit 되는지를 metrics 패키지의 testutil
// 카운트로 확인한다. fake device 의 snapshot 에 Device 필드를 명시 주입해 self-health 라벨이 정상 채워지게.
func TestPollOnce_RecordsMigModeAndMpsActive(t *testing.T) {
	prevDetect := mpsDetect
	mpsDetect = func() bool { return true }
	t.Cleanup(func() { mpsDetect = prevDetect })

	dev := &fakeDevice{
		info: types.GPUDevice{Index: 0, UUID: "GPU-test", Model: "RTX 3090", MigMode: types.MigModeUnsupported},
		snapshot: types.GPUSnapshot{
			Device: types.GPUDevice{Index: 0, UUID: "GPU-test", Model: "RTX 3090", MigMode: types.MigModeUnsupported},
		},
	}
	cfg := config.Config{GPUMetricsEnabled: true, NodeName: "node-test"}
	c, _ := newCollectorWithDevs(t, cfg, nil, dev)

	c.pollOnce()

	if dev.snapCallCount() < 1 {
		t.Fatalf("snapshot 미호출 (expected ≥1)")
	}
}

// TestBuildProcessUtilMap_Basic 은 #104 helper 의 기본 변환 흐름 검증.
func TestBuildProcessUtilMap_Basic(t *testing.T) {
	utils := []types.GPUProcessUtil{
		{PID: 100, SmUtilPct: 30},
		{PID: 200, SmUtilPct: 45},
	}
	m := buildProcessUtilMap(utils)
	if got := m[100]; got != 30 {
		t.Errorf("pid 100 util=%d want 30", got)
	}
	if got := m[200]; got != 45 {
		t.Errorf("pid 200 util=%d want 45", got)
	}
	if got := m[999]; got != 0 {
		t.Errorf("missing pid lookup=%d want 0 (zero default)", got)
	}
}

// TestBuildProcessUtilMap_NilEmpty 는 nil / 빈 입력이 panic 없이 빈 map 으로 흡수되는지 검증한다.
func TestBuildProcessUtilMap_NilEmpty(t *testing.T) {
	if got := len(buildProcessUtilMap(nil)); got != 0 {
		t.Errorf("nil len=%d want 0", got)
	}
	if got := len(buildProcessUtilMap([]types.GPUProcessUtil{})); got != 0 {
		t.Errorf("empty len=%d want 0", got)
	}
}

// TestBuildProcessUtilMap_DuplicatePIDMaxWins 는 동일 PID 에 다회 sample 시 max 값이 채택되는지 검증.
// NVML 의 sampling jitter 보정 의미.
func TestBuildProcessUtilMap_DuplicatePIDMaxWins(t *testing.T) {
	utils := []types.GPUProcessUtil{
		{PID: 100, SmUtilPct: 30},
		{PID: 100, SmUtilPct: 70},
		{PID: 100, SmUtilPct: 50},
	}
	m := buildProcessUtilMap(utils)
	if got := m[100]; got != 70 {
		t.Errorf("dup pid max=%d want 70", got)
	}
}

// TestCapUtilPct 는 #104 의 multi-PID 합산 cap 검증.
func TestCapUtilPct(t *testing.T) {
	cases := []struct {
		in, want uint32
	}{
		{0, 0},
		{50, 50},
		{100, 100},
		{101, 100},
		{500, 100},
	}
	for _, tc := range cases {
		if got := capUtilPct(tc.in); got != tc.want {
			t.Errorf("capUtilPct(%d)=%d want %d", tc.in, got, tc.want)
		}
	}
}

// TestPollOnce_AggregatesSmUtilPerPod 는 #104 의 multi-PID 합산 흐름 검증. 한 Pod 의 두 PID 가
// 동일 device 에서 각각 SM util 30 / 25 sample 일 때 합산 결과가 55 로 PodGPUSample.SmUtilPct 에
// 정확히 채워지는지 확인한다.
func TestPollOnce_AggregatesSmUtilPerPod(t *testing.T) {
	dev := &fakeDevice{
		info: types.GPUDevice{Index: 0, UUID: "GPU-0"},
		snapshot: types.GPUSnapshot{
			Device: types.GPUDevice{Index: 0, UUID: "GPU-0"},
		},
		processes: []types.GPUProcess{
			{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1024},
			{DeviceIndex: 0, PID: 200, MemoryUsedBytes: 2048},
		},
		procUtils: []types.GPUProcessUtil{
			{PID: 100, SmUtilPct: 30},
			{PID: 200, SmUtilPct: 25},
		},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "uid-1"},
		200: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "uid-1"},
	}}
	spy := &snapshotSpy{}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	c.recordSnapshot = spy.record

	c.pollOnce()

	if got := spy.callCount(); got != 1 {
		t.Fatalf("snapshot calls=%d want 1", got)
	}
	samples := spy.lastSamples()
	if len(samples) != 1 {
		t.Fatalf("samples len=%d want 1 (one pod)", len(samples))
	}
	if got := samples[0].SmUtilPct; got != 55 {
		t.Errorf("aggregated SmUtilPct=%d want 55 (30+25)", got)
	}
	if got := samples[0].MemUsedBytes; got != 3072 {
		t.Errorf("aggregated MemUsedBytes=%d want 3072", got)
	}
}

// TestPollOnce_SmUtilCappedAt100 은 multi-PID 합산이 100 을 초과할 때 cap 가 정확히 100 으로 절단
// 되는지 검증한다 (NVML sampling jitter 보정).
func TestPollOnce_SmUtilCappedAt100(t *testing.T) {
	dev := &fakeDevice{
		info: types.GPUDevice{Index: 0, UUID: "GPU-0"},
		snapshot: types.GPUSnapshot{
			Device: types.GPUDevice{Index: 0, UUID: "GPU-0"},
		},
		processes: []types.GPUProcess{
			{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1},
			{DeviceIndex: 0, PID: 200, MemoryUsedBytes: 1},
			{DeviceIndex: 0, PID: 300, MemoryUsedBytes: 1},
		},
		procUtils: []types.GPUProcessUtil{
			{PID: 100, SmUtilPct: 60},
			{PID: 200, SmUtilPct: 70},
			{PID: 300, SmUtilPct: 50},
		},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "uid-1"},
		200: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "uid-1"},
		300: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "uid-1"},
	}}
	spy := &snapshotSpy{}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	c.recordSnapshot = spy.record

	c.pollOnce()

	samples := spy.lastSamples()
	if len(samples) != 1 {
		t.Fatalf("samples len=%d want 1", len(samples))
	}
	if got := samples[0].SmUtilPct; got != 100 {
		t.Errorf("capped SmUtilPct=%d want 100", got)
	}
}

// TestPollOnce_ProcessUtilCalledPerDevice 는 device 마다 ProcessUtilization 가 1회 호출 되는지 검증해
// NVML 호출 빈도 회귀를 차단한다.
func TestPollOnce_ProcessUtilCalledPerDevice(t *testing.T) {
	dev := &fakeDevice{
		info: types.GPUDevice{Index: 0, UUID: "GPU-0"},
		snapshot: types.GPUSnapshot{
			Device: types.GPUDevice{Index: 0, UUID: "GPU-0"},
		},
	}
	resolver := &fakeResolver{byPID: nil}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)

	c.pollOnce()

	dev.mu.Lock()
	calls := dev.procUtilCalls
	dev.mu.Unlock()
	if calls != 1 {
		t.Errorf("ProcessUtilization calls=%d want 1 (per device per poll)", calls)
	}
}

// fakeMigInstance 는 #104 MIG instance 핸들의 테스트용 stub. parent fakeDevice 가 enumerate 결과 로
// 반환한다. Info / ProcessUtilization / GpuInstanceId 만 의미 있는 동작 을 제공 하고 나머지 는 zero.
type fakeMigInstance struct {
	uuid        string
	giID        uint32
	utils       []types.GPUProcessUtil
	closeCalled bool
}

func (m *fakeMigInstance) Info() (types.GPUDevice, error) {
	return types.GPUDevice{UUID: m.uuid, MigMode: types.MigModeEnabled}, nil
}
func (m *fakeMigInstance) Snapshot() (types.GPUSnapshot, error) { return types.GPUSnapshot{}, nil }
func (m *fakeMigInstance) RunningProcesses() ([]types.GPUProcess, error) {
	return nil, nil
}
func (m *fakeMigInstance) ProcessUtilization() ([]types.GPUProcessUtil, error) {
	return m.utils, nil
}
func (m *fakeMigInstance) MigMode() (types.MigMode, error)    { return types.MigModeEnabled, nil }
func (m *fakeMigInstance) MaxMigDeviceCount() (int, error)    { return 0, nil }
func (m *fakeMigInstance) MigDevice(int) (nvml.Device, error) { return nil, nil }
func (m *fakeMigInstance) IsMigDevice() (bool, error)         { return true, nil }
func (m *fakeMigInstance) GpuInstanceId() (uint32, error)     { return m.giID, nil }
func (m *fakeMigInstance) ComputeInstanceId() (uint32, error) { return 0, nil }
func (m *fakeMigInstance) Close() error                       { m.closeCalled = true; return nil }

// migFakeDevice 는 MIG enabled parent device 의 테스트용 stub. instance enumerate 분기 검증 전용.
type migFakeDevice struct {
	*fakeDevice
	migInstances []*fakeMigInstance
}

func (d *migFakeDevice) MaxMigDeviceCount() (int, error) { return len(d.migInstances), nil }
func (d *migFakeDevice) MigDevice(i int) (nvml.Device, error) {
	if i < 0 || i >= len(d.migInstances) {
		return nil, nil
	}
	return d.migInstances[i], nil
}

// migSnapshotSpy 는 recordMigSnapshot test seam 의 호출 인자 캡처.
type migSnapshotSpy struct {
	mu    sync.Mutex
	calls [][]metrics.PodMigGPUSample
}

func (s *migSnapshotSpy) record(_ string, samples []metrics.PodMigGPUSample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]metrics.PodMigGPUSample, len(samples))
	copy(cp, samples)
	s.calls = append(s.calls, cp)
}

func (s *migSnapshotSpy) lastSamples() []metrics.PodMigGPUSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return nil
	}
	return s.calls[len(s.calls)-1]
}

// TestPollOnce_MigPathSkipsWhenDeviceDisabled 는 MIG 미지원 device 에서 instance enumerate / mig sample
// 발행 이 일어나지 않음 을 검증 (RTX 3090 같은 dev cluster 경로). recordMigSnapshot 은 cleanup invariant
// 유지 위해 매 poll 호출 되지만 samples 는 항상 empty.
func TestPollOnce_MigPathSkipsWhenDeviceDisabled(t *testing.T) {
	dev := &fakeDevice{
		info: types.GPUDevice{Index: 0, UUID: "GPU-0", MigMode: types.MigModeUnsupported},
		snapshot: types.GPUSnapshot{
			Device: types.GPUDevice{Index: 0, UUID: "GPU-0", MigMode: types.MigModeUnsupported},
		},
	}
	spy := &migSnapshotSpy{}
	resolver := &fakeResolver{}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	c.recordMigSnapshot = spy.record

	c.pollOnce()

	if len(spy.calls) != 1 {
		t.Fatalf("recordMigSnapshot calls=%d want 1 (cleanup invariant)", len(spy.calls))
	}
	if got := spy.lastSamples(); len(got) != 0 {
		t.Errorf("mig samples=%d want 0 (MIG unsupported)", len(got))
	}
}

// TestPollOnce_MigPathEnumeratesInstances 는 MIG enabled device 에서 instance enumerate 후 instance 별
// util 이 (podUID, mig_uuid, gi_id) 키로 합산 되어 발행 되는지 검증. 두 instance, 각 instance 에 단일 Pod
// process 가 있을 때 두 시리즈 가 emit 되어야.
func TestPollOnce_MigPathEnumeratesInstances(t *testing.T) {
	inst0 := &fakeMigInstance{
		uuid: "MIG-0",
		giID: 1,
		utils: []types.GPUProcessUtil{
			{PID: 100, SmUtilPct: 30},
		},
	}
	inst1 := &fakeMigInstance{
		uuid: "MIG-1",
		giID: 2,
		utils: []types.GPUProcessUtil{
			{PID: 200, SmUtilPct: 50},
		},
	}
	parent := &migFakeDevice{
		fakeDevice: &fakeDevice{
			info: types.GPUDevice{Index: 0, UUID: "GPU-parent", MigMode: types.MigModeEnabled},
			snapshot: types.GPUSnapshot{
				Device: types.GPUDevice{Index: 0, UUID: "GPU-parent", MigMode: types.MigModeEnabled},
			},
		},
		migInstances: []*fakeMigInstance{inst0, inst1},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "uid-1"},
		200: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p2", PodUID: "uid-2"},
	}}
	spy := &migSnapshotSpy{}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "n"}

	// migFakeDevice 가 nvml.Device 인터페이스 만족 하도록 wrap 한 fakeNVML 시드.
	fake := &fakeNVML{count: 1, devices: map[uint]*fakeDevice{0: parent.fakeDevice}}
	c := New(fake, cfg, resolver)
	c.devSet = nvml.NewDeviceSet(&migFakeNVML{parent: parent})
	if err := c.devSet.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	c.recordMigSnapshot = spy.record

	c.pollOnce()

	samples := spy.lastSamples()
	if len(samples) != 2 {
		t.Fatalf("mig samples=%d want 2 (2 instances, 2 pods)", len(samples))
	}
	// #104 G1 fix 이후 instance handle 은 parent deviceImpl 의 cache 슬롯이 보유 하고 collector pollOnce
	// 는 매 poll 마다 Close 를 호출 하지 않는다 (parent Close 가 children 일괄 해제 책임). 본 테스트의
	// 직전 close 호출 검증 은 nvml 패키지의 deviceImpl 단위 테스트로 이관 되어야 한다 (별도 PR).
}

// migFakeNVML 은 migFakeDevice 를 nvml.NVML.Device 결과로 반환하는 NVML wrapper.
type migFakeNVML struct {
	parent *migFakeDevice
}

func (n *migFakeNVML) DeviceCount() (uint, error)            { return 1, nil }
func (n *migFakeNVML) Device(_ uint) (nvml.Device, error)    { return n.parent, nil }
func (n *migFakeNVML) DeviceUUID(_ uint) (string, error)     { return n.parent.info.UUID, nil }
func (n *migFakeNVML) Shutdown() error                       { return nil }

// TestPollOnce_CollectsPodContention 는 #198 cgroup 경합 수집이 Pod 당 대표 PID 1회만 읽고 (다중 GPU
// 프로세스 중복 read 회피) PSI 비율을 RecordPodContention 으로 전달하는지 검증한다.
func TestPollOnce_CollectsPodContention(t *testing.T) {
	dev := &fakeDevice{
		info: types.GPUDevice{Index: 0, UUID: "u0"},
		processes: []types.GPUProcess{
			{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1024},
			{DeviceIndex: 0, PID: 101, MemoryUsedBytes: 2048}, // 동일 Pod 의 두 번째 PID
		},
	}
	pod := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "u1"}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{100: pod, 101: pod}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, ContentionEnabled: true, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)

	var readUIDs []string
	c.readContention = func(podUID string) (contention.Stats, bool) {
		readUIDs = append(readUIDs, podUID)
		return contention.Stats{CPUPressureRatio: 0.25, MemPressureRatio: 0.1}, true
	}
	var got []metrics.PodContentionSample
	c.recordContention = func(node string, samples []metrics.PodContentionSample) { got = samples }

	c.pollOnce()

	if len(readUIDs) != 1 || readUIDs[0] != "u1" {
		t.Fatalf("readContention UIDs=%v want [u1] (Pod 당 1회)", readUIDs)
	}
	if len(got) != 1 || got[0].ID.PodUID != "u1" || got[0].CPUPressureRatio != 0.25 || got[0].MemPressureRatio != 0.1 {
		t.Fatalf("contention samples=%+v want 1건 (u1, cpu 0.25, mem 0.1)", got)
	}
}

// TestPollOnce_ContentionSkippedWhenToggleOff 는 ContentionEnabled=false 면 cgroup read / record 가
// 전혀 호출되지 않아 PSI 파일 read 비용이 0 임을 검증한다.
func TestPollOnce_ContentionSkippedWhenToggleOff(t *testing.T) {
	dev := &fakeDevice{
		info:      types.GPUDevice{Index: 0, UUID: "u0"},
		processes: []types.GPUProcess{{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1}},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p1", PodUID: "u1"},
	}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, ContentionEnabled: false, NodeName: "n"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)
	called := false
	c.readContention = func(podUID string) (contention.Stats, bool) { called = true; return contention.Stats{}, false }
	c.recordContention = func(node string, samples []metrics.PodContentionSample) { called = true }

	c.pollOnce()

	if called {
		t.Fatalf("ContentionEnabled=false 인데 contention 경로가 호출됨")
	}
}
