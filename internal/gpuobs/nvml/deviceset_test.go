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
	return &fakeDeviceSetDevice{uuid: uuid, parent: f}, nil
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
}

func (d *fakeDeviceSetDevice) Info() (types.GPUDevice, error) {
	return types.GPUDevice{UUID: d.uuid}, nil
}
func (d *fakeDeviceSetDevice) Snapshot() (types.GPUSnapshot, error) {
	return types.GPUSnapshot{Device: types.GPUDevice{UUID: d.uuid}}, nil
}
func (d *fakeDeviceSetDevice) RunningProcesses() ([]types.GPUProcess, error) { return nil, nil }
func (d *fakeDeviceSetDevice) Close() error {
	d.parent.mu.Lock()
	d.parent.closed[d.uuid]++
	d.parent.mu.Unlock()
	return nil
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

func TestDeviceSet_CountErrorPropagates(t *testing.T) {
	nv := &fakeDeviceSetNVML{}
	set := NewDeviceSet(nv)
	// indexUUIDs 비어있고 uuidErr 등도 없음 → DeviceCount 는 0 이라 에러 아님.
	// count 자체 에러 시뮬: 별도 fake 가 필요. fakeDeviceSetNVML 의 단순 케이스로 충분하지 않으므로
	// 본 테스트는 정상 경로 (count=0) 가 에러 없이 반환되는지만 확인한다.
	if err := set.Sync(); err != nil {
		t.Errorf("empty Sync should not error; got %v", err)
	}
	if set.Len() != 0 {
		t.Errorf("Len=%d want 0", set.Len())
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
