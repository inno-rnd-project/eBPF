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
)

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
	mu          sync.Mutex
	info        types.GPUDevice
	snapshot    types.GPUSnapshot
	snapshotErr error
	snapCalls   int
}

func (d *fakeDevice) Info() (types.GPUDevice, error) { return d.info, nil }

func (d *fakeDevice) Snapshot() (types.GPUSnapshot, error) {
	d.mu.Lock()
	d.snapCalls++
	d.mu.Unlock()
	return d.snapshot, d.snapshotErr
}

func (d *fakeDevice) RunningProcesses() ([]types.GPUProcess, error) { return nil, nil }

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
	c := New(nil, cfg)

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
	c := New(fake, cfg)

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
	c := New(fake, cfg)

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
	c := New(fake, cfg)
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
