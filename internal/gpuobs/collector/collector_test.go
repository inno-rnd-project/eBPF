package collector

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"netobs/internal/gpuobs/config"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// fakeResolver는 PID → PodIdentity 매핑을 테스트에서 직접 시드하는 PodResolver 구현이다.
// 등록되지 않은 PID는 unresolved를 반환해 RecordPod의 IsPod 가드에서 자연스럽게 걸러진다.
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
	mu           sync.Mutex
	info         types.GPUDevice
	snapshot     types.GPUSnapshot
	snapshotErr  error
	snapCalls    int
	processes    []types.GPUProcess
	processesErr error
	procCalls    int
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
}

func TestPollOnce_PerDeviceErrorContinues(t *testing.T) {
	dev0 := &fakeDevice{info: types.GPUDevice{Index: 0, UUID: "u0"}}
	dev1 := &fakeDevice{info: types.GPUDevice{Index: 1, UUID: "u1"}, snapshotErr: errors.New("boom")}
	dev2 := &fakeDevice{info: types.GPUDevice{Index: 2, UUID: "u2"}}
	fake := &fakeNVML{count: 3, devices: map[uint]*fakeDevice{0: dev0, 1: dev1, 2: dev2}}
	cfg := config.Config{GPUMetricsEnabled: true, NodeName: "n"}
	c := New(fake, cfg, nil)
	// Run을 거치지 않고 pollOnce만 단독 검증하므로 discover가 채워야 할 devices를 직접 주입한다.
	c.devices = []nvml.Device{dev0, dev1, dev2}

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
	c := New(nil, cfg, resolver)
	c.devices = []nvml.Device{dev}

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
	c := New(nil, cfg, nil)
	c.devices = []nvml.Device{dev}

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
	c := New(nil, cfg, resolver)
	c.devices = []nvml.Device{dev}

	c.pollOnce()

	if got := dev.procCallCount(); got != 0 {
		t.Errorf("disabled toggle must short-circuit RunningProcesses; got %d calls", got)
	}
	if got := resolver.callCount(); got != 0 {
		t.Errorf("disabled toggle must short-circuit ResolvePID; got %d calls", got)
	}
}
