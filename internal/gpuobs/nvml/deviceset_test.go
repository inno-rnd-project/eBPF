package nvml

import (
	"errors"
	"sort"
	"sync"
	"testing"

	"netobs/internal/gpuobs/types"
)

// fakeDeviceSetNVML 은 DeviceSet 의 NVML 의존성을 단위 테스트로 격리하기 위한 fake 다.
// indexUUIDs 는 NVML 이 보고하는 (index → UUID) 매핑을 모사하며, 테스트가 hot-add / hot-remove /
// reorder 시나리오를 만들기 위해 직접 갱신한다. opens 는 nv.Device 가 호출된 횟수 (UUID 별) 를 추적해
// DeviceSet 이 동일 UUID 에 대해 핸들을 재사용하는지 검증한다.
type fakeDeviceSetNVML struct {
	mu         sync.Mutex
	indexUUIDs []string
	openErr    map[string]error
	uuidErr    map[uint]error
	countErr   error
	opens      map[string]int
	closed     map[string]int
}

func newFakeDeviceSetNVML(uuids ...string) *fakeDeviceSetNVML {
	return &fakeDeviceSetNVML{
		indexUUIDs: append([]string(nil), uuids...),
		opens:      make(map[string]int),
		closed:     make(map[string]int),
		openErr:    make(map[string]error),
		uuidErr:    make(map[uint]error),
	}
}

func (f *fakeDeviceSetNVML) setIndexUUIDs(uuids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexUUIDs = append([]string(nil), uuids...)
}

func (f *fakeDeviceSetNVML) DeviceCount() (uint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.countErr != nil {
		return 0, f.countErr
	}
	return uint(len(f.indexUUIDs)), nil
}

func (f *fakeDeviceSetNVML) DeviceUUID(index uint) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.uuidErr[index]; ok {
		return "", err
	}
	if int(index) >= len(f.indexUUIDs) {
		return "", errors.New("out of range")
	}
	return f.indexUUIDs[index], nil
}

func (f *fakeDeviceSetNVML) Device(index uint) (Device, error) {
	f.mu.Lock()
	if int(index) >= len(f.indexUUIDs) {
		f.mu.Unlock()
		return nil, errors.New("out of range")
	}
	uuid := f.indexUUIDs[index]
	if err, ok := f.openErr[uuid]; ok {
		f.mu.Unlock()
		return nil, err
	}
	f.opens[uuid]++
	f.mu.Unlock()
	return &fakeDeviceSetDevice{uuid: uuid, index: index, parent: f}, nil
}

func (f *fakeDeviceSetNVML) Shutdown() error { return nil }

func (f *fakeDeviceSetNVML) openCount(uuid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[uuid]
}

func (f *fakeDeviceSetNVML) closeCount(uuid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[uuid]
}

type fakeDeviceSetDevice struct {
	uuid   string
	parent *fakeDeviceSetNVML

	// index 는 운영 deviceImpl 의 currentIdx 와 동등한 hot-plug 추적 슬롯이다. updateIndex 호출 시 갱신되고
	// Info() 가 매 호출마다 반영해 반환한다. parent.mu 와 별도 lock 없이 parent.mu 를 그대로 빌려 보호한다.
	index uint

	// infoUUID 는 Info() 가 반환하는 UUID 의 오버라이드다. DeviceUUID(i) 와 Device(i) 사이 race 를 시뮬레이션할 때
	// (DeviceSet 이 받은 dev.Info().UUID 가 기대 UUID 와 다른 케이스) 호출자가 본 필드를 set 한다. 빈 값이면
	// d.uuid 가 그대로 반환된다.
	infoUUID string
}

func (d *fakeDeviceSetDevice) Info() (types.GPUDevice, error) {
	d.parent.mu.Lock()
	defer d.parent.mu.Unlock()
	uuid := d.uuid
	if d.infoUUID != "" {
		uuid = d.infoUUID
	}
	return types.GPUDevice{Index: d.index, UUID: uuid}, nil
}
func (d *fakeDeviceSetDevice) Snapshot() (types.GPUSnapshot, error) {
	info, _ := d.Info()
	return types.GPUSnapshot{Device: info}, nil
}
func (d *fakeDeviceSetDevice) RunningProcesses() ([]types.GPUProcess, error) { return nil, nil }
func (d *fakeDeviceSetDevice) Close() error {
	d.parent.mu.Lock()
	d.parent.closed[d.uuid]++
	d.parent.mu.Unlock()
	return nil
}

// updateIndex 는 운영 deviceImpl 의 동명 메서드와 같은 시그니처를 구현해, DeviceSet.Sync 가 type assertion
// 으로 호출하는 indexUpdater 인터페이스를 fake 도 만족시키도록 한다. 이로써 reorder 시나리오 단위 테스트가
// Info().Index 의 hot-plug 추적을 검증할 수 있다.
func (d *fakeDeviceSetDevice) updateIndex(newIndex uint) {
	d.parent.mu.Lock()
	d.index = newIndex
	d.parent.mu.Unlock()
}

func snapshotUUIDs(set *DeviceSet) []string {
	devs := set.Snapshot()
	out := make([]string, 0, len(devs))
	for _, d := range devs {
		info, _ := d.Info()
		out = append(out, info.UUID)
	}
	sort.Strings(out)
	return out
}

func TestDeviceSet_FirstSyncRegistersAllDevices(t *testing.T) {
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	set := NewDeviceSet(nv)

	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got := snapshotUUIDs(set)
	want := []string{"GPU-A", "GPU-B"}
	if !equalStrings(got, want) {
		t.Errorf("snapshot=%v want %v", got, want)
	}
	if nv.openCount("GPU-A") != 1 || nv.openCount("GPU-B") != 1 {
		t.Errorf("each UUID must be Opened exactly once on first Sync; opens=%v", nv.opens)
	}
}

func TestDeviceSet_SyncReusesHandlesForExistingUUIDs(t *testing.T) {
	// 동일 UUID 가 두 번째 Sync 에서 다시 보고되어도 Device(index) 가 재호출되지 않아야 한다.
	// 데이터센터 GPU 의 경우 Device(index) 가 GPM init 등 비싼 동작을 수행하므로 idempotent 가 중요하다.
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	set := NewDeviceSet(nv)

	if err := set.Sync(); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if err := set.Sync(); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	if nv.openCount("GPU-A") != 1 {
		t.Errorf("GPU-A opens=%d want 1 (handle must be reused)", nv.openCount("GPU-A"))
	}
	if nv.openCount("GPU-B") != 1 {
		t.Errorf("GPU-B opens=%d want 1 (handle must be reused)", nv.openCount("GPU-B"))
	}
}

func TestDeviceSet_HotAddDetectsNewUUID(t *testing.T) {
	nv := newFakeDeviceSetNVML("GPU-A")
	set := NewDeviceSet(nv)
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	nv.setIndexUUIDs("GPU-A", "GPU-B")
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync after add: %v", err)
	}

	got := snapshotUUIDs(set)
	if !equalStrings(got, []string{"GPU-A", "GPU-B"}) {
		t.Errorf("after hot-add snapshot=%v want [GPU-A GPU-B]", got)
	}
	if nv.openCount("GPU-A") != 1 {
		t.Errorf("existing GPU-A must not be re-opened; opens=%d", nv.openCount("GPU-A"))
	}
	if nv.openCount("GPU-B") != 1 {
		t.Errorf("new GPU-B must be opened once; opens=%d", nv.openCount("GPU-B"))
	}
}

func TestDeviceSet_HotRemoveClosesAndDropsUUID(t *testing.T) {
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	set := NewDeviceSet(nv)
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	nv.setIndexUUIDs("GPU-A")
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync after remove: %v", err)
	}

	got := snapshotUUIDs(set)
	if !equalStrings(got, []string{"GPU-A"}) {
		t.Errorf("after hot-remove snapshot=%v want [GPU-A]", got)
	}
	if nv.closeCount("GPU-B") != 1 {
		t.Errorf("removed GPU-B must be Closed; closes=%d", nv.closeCount("GPU-B"))
	}
	if nv.closeCount("GPU-A") != 0 {
		t.Errorf("surviving GPU-A must not be Closed; closes=%d", nv.closeCount("GPU-A"))
	}
}

func TestDeviceSet_ReorderKeepsHandlesByUUID(t *testing.T) {
	// hot-remove 후 NVML 이 remaining device 의 index 를 재배치할 때, 본 셋은 UUID 기준이라 영향이 없어야 한다.
	// GPU-A, GPU-B → GPU-A 제거 후 NVML 이 GPU-B 를 index 0 으로 보고해도 GPU-B handle 은 그대로 유지된다.
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	set := NewDeviceSet(nv)
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	nv.setIndexUUIDs("GPU-B") // index 0 가 이전엔 GPU-A 였는데 이제 GPU-B
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync after reorder: %v", err)
	}

	got := snapshotUUIDs(set)
	if !equalStrings(got, []string{"GPU-B"}) {
		t.Errorf("after reorder snapshot=%v want [GPU-B]", got)
	}
	if nv.openCount("GPU-B") != 1 {
		t.Errorf("GPU-B must be Opened only once across reorder; opens=%d", nv.openCount("GPU-B"))
	}
	if nv.closeCount("GPU-A") != 1 {
		t.Errorf("GPU-A must be Closed once after disappearing; closes=%d", nv.closeCount("GPU-A"))
	}
}

func TestDeviceSet_PartialFailureOnOpenSkipsAndContinues(t *testing.T) {
	// 한 device 의 Open 이 실패해도 나머지 device 의 sync 는 계속 되어야 한다.
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	nv.openErr["GPU-B"] = errors.New("simulated open error")
	set := NewDeviceSet(nv)

	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got := snapshotUUIDs(set)
	if !equalStrings(got, []string{"GPU-A"}) {
		t.Errorf("partial-failure sync=%v want [GPU-A]", got)
	}
}

func TestDeviceSet_PartialFailureOnUUIDLookupSkips(t *testing.T) {
	// 한 index 의 UUID 조회가 실패해도 다른 index 는 정상 등록되어야 한다.
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	nv.uuidErr[1] = errors.New("simulated uuid error")
	set := NewDeviceSet(nv)

	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got := snapshotUUIDs(set)
	if !equalStrings(got, []string{"GPU-A"}) {
		t.Errorf("partial-failure sync=%v want [GPU-A]", got)
	}
}

func TestDeviceSet_CloseClosesAllAndEmptiesSet(t *testing.T) {
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	set := NewDeviceSet(nv)
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if err := set.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if nv.closeCount("GPU-A") != 1 {
		t.Errorf("GPU-A close=%d want 1", nv.closeCount("GPU-A"))
	}
	if nv.closeCount("GPU-B") != 1 {
		t.Errorf("GPU-B close=%d want 1", nv.closeCount("GPU-B"))
	}
	if set.Len() != 0 {
		t.Errorf("after Close Len=%d want 0", set.Len())
	}
}

func TestDeviceSet_EmptySyncDoesNotError(t *testing.T) {
	// indexUUIDs 가 비어 있으면 DeviceCount 는 0 을 반환하고, 빈 셋의 Sync 도 에러 없이 종료되어야 한다.
	nv := newFakeDeviceSetNVML()
	set := NewDeviceSet(nv)

	if err := set.Sync(); err != nil {
		t.Errorf("empty Sync should not error; got %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("Len=%d want 0", set.Len())
	}
}

func TestDeviceSet_CountErrorPropagates(t *testing.T) {
	// DeviceCount 자체가 에러를 반환하면 그 에러가 호출자에게 그대로 전파되고 셋 상태는 변경되지 않아야 한다.
	nv := newFakeDeviceSetNVML("GPU-A")
	if err := (NewDeviceSet(nv)).Sync(); err != nil {
		t.Fatalf("baseline Sync should succeed; got %v", err)
	}

	// 새 셋을 만들어 countErr 만 시뮬한다 — 기존 byUUID 와 분리해 에러 전파 자체에 집중한다.
	nv2 := newFakeDeviceSetNVML("GPU-A")
	nv2.countErr = errors.New("simulated count error")
	set := NewDeviceSet(nv2)

	err := set.Sync()
	if err == nil || err.Error() != "simulated count error" {
		t.Errorf("Sync should propagate DeviceCount error verbatim; got %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("Len=%d want 0 (count error must not register devices)", set.Len())
	}
}

func TestDeviceSet_PartialFailureSkipsCleanup(t *testing.T) {
	// 한 device 의 UUID lookup 이 일시 실패한 cycle 에서는 stale cleanup 자체가 건너뛰어야 한다.
	// 이로써 정상 device 가 transient error 에 휩쓸려 Close → 다음 cycle 재 Open 으로 churn 하지 않는다.
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	set := NewDeviceSet(nv)
	if err := set.Sync(); err != nil {
		t.Fatalf("baseline Sync: %v", err)
	}

	// 두 번째 sync 에서 idx=0 (GPU-A) 의 UUID lookup 만 일시 실패한다고 시뮬.
	nv.uuidErr[0] = errors.New("transient")
	if err := set.Sync(); err != nil {
		t.Fatalf("partial-failure Sync should not return error; got %v", err)
	}

	// 정상 동작이라면 GPU-A 가 seen 에서 빠져 stale 정리되었을 것. cleanup skip 으로 Close 호출이 없어야 한다.
	if got := nv.closeCount("GPU-A"); got != 0 {
		t.Errorf("transient UUID lookup failure must not Close existing device; closes=%d", got)
	}
	if got := snapshotUUIDs(set); !equalStrings(got, []string{"GPU-A", "GPU-B"}) {
		t.Errorf("set must retain both UUIDs across transient failure; got %v", got)
	}

	// transient 해제 후 다음 sync 에서 정상 cleanup 이 가능한 상태로 돌아오는지도 확인한다.
	delete(nv.uuidErr, 0)
	nv.setIndexUUIDs("GPU-B") // 이제 진짜 GPU-A 가 사라진 상태
	if err := set.Sync(); err != nil {
		t.Fatalf("recovery Sync: %v", err)
	}
	if got := nv.closeCount("GPU-A"); got != 1 {
		t.Errorf("after recovery sync, GPU-A should be Closed exactly once; closes=%d", got)
	}
}

func TestDeviceSet_UUIDMismatchRaceClosesDevAndSkips(t *testing.T) {
	// DeviceUUID(i) 와 Device(i) 사이 race 시뮬: NVML 이 idx=0 의 UUID 로 GPU-A 를 보고했지만
	// Device(i) 가 돌려준 핸들의 Info().UUID 는 GPU-X (race 로 바뀐 상태). DeviceSet 은 dev 를 close 하고 등록을 거부해야 한다.
	nv := newFakeDeviceSetNVML("GPU-A")
	// indexUUIDs 는 GPU-A 라고 보고하지만, fakeDevice 의 Info() 가 다른 UUID 를 돌려주도록 후처리.
	set := NewDeviceSet(nv)

	// 첫 sync 가 device 를 Open 한 직후, race 시뮬을 위해 그 device 의 infoUUID 를 GPU-X 로 바꾼다.
	// 단, DeviceSet 은 첫 sync 에서 mismatch 를 감지하기 위해 sync 도중에 mismatch 가 발생해야 한다.
	// 본 fake 는 Open 직후 부터 infoUUID 를 set 해 둘 수 없으므로, 시뮬 우회로: fakeDeviceSetNVML 의 Device 가
	// 갓 Open 한 dev 에 대해 infoUUID 를 미리 주입할 수 있도록 별도 hook 가 필요하지만, 본 테스트는
	// 원리적 검증을 위해 별도 fake (fakeRaceNVML) 를 사용한다.
	nv2 := &fakeRaceNVML{
		realUUID:  "GPU-A",
		fakeInfoUUID: "GPU-X",
		parent:    nv,
	}
	set = NewDeviceSet(nv2)
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	if set.Len() != 0 {
		t.Errorf("UUID mismatch must reject registration; Len=%d want 0", set.Len())
	}
	if nv2.closeCount != 1 {
		t.Errorf("mismatched dev must be Closed; closeCount=%d want 1", nv2.closeCount)
	}
}

// fakeRaceNVML 은 DeviceUUID 가 보고한 UUID 와 Device 가 돌려주는 dev 의 Info().UUID 가 다른 race 상황을
// 시뮬레이션한다. 단순 fakeDeviceSetNVML 만으로는 Open 시점에 infoUUID 를 주입할 수 있는 hook 이 없어 별도로 둔다.
type fakeRaceNVML struct {
	realUUID     string
	fakeInfoUUID string
	closeCount   int
	parent       *fakeDeviceSetNVML
}

func (f *fakeRaceNVML) DeviceCount() (uint, error)            { return 1, nil }
func (f *fakeRaceNVML) DeviceUUID(i uint) (string, error)     { return f.realUUID, nil }
func (f *fakeRaceNVML) Shutdown() error                       { return nil }
func (f *fakeRaceNVML) Device(i uint) (Device, error) {
	return &fakeRaceDevice{uuid: f.fakeInfoUUID, parent: f}, nil
}

type fakeRaceDevice struct {
	uuid   string
	parent *fakeRaceNVML
}

func (d *fakeRaceDevice) Info() (types.GPUDevice, error) {
	return types.GPUDevice{UUID: d.uuid}, nil
}
func (d *fakeRaceDevice) Snapshot() (types.GPUSnapshot, error) {
	return types.GPUSnapshot{Device: types.GPUDevice{UUID: d.uuid}}, nil
}
func (d *fakeRaceDevice) RunningProcesses() ([]types.GPUProcess, error) { return nil, nil }
func (d *fakeRaceDevice) Close() error                                  { d.parent.closeCount++; return nil }

func TestDeviceSet_ReorderUpdatesIndex(t *testing.T) {
	// hot-remove 후 NVML 이 remaining device 의 index 를 재배치할 때, 본 셋은 기존 UUID 의 index 를
	// updateIndex 로 갱신해 Info().Index 가 stale 되지 않게 해야 한다.
	nv := newFakeDeviceSetNVML("GPU-A", "GPU-B")
	set := NewDeviceSet(nv)
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// GPU-A 가 사라지고 GPU-B 가 idx 0 으로 재배치된 상태.
	nv.setIndexUUIDs("GPU-B")
	if err := set.Sync(); err != nil {
		t.Fatalf("Sync after reorder: %v", err)
	}

	// snapshot 의 Device.Index 가 1 (이전 값) 이 아닌 0 (현재 NVML index) 이어야 한다.
	devs := set.Snapshot()
	if len(devs) != 1 {
		t.Fatalf("after reorder Len=%d want 1", len(devs))
	}
	info, _ := devs[0].Info()
	if info.UUID != "GPU-B" {
		t.Fatalf("survived UUID=%s want GPU-B", info.UUID)
	}
	if info.Index != 0 {
		t.Errorf("Info().Index=%d want 0 (NVML index reorder must be reflected via updateIndex)", info.Index)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
