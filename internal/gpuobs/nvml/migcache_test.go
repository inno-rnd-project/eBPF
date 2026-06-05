package nvml

import (
	"testing"

	"netobs/internal/gpuobs/types"
)

// TestDeviceImpl_MigDeviceCacheReturnsSameHandle 는 #104 G1 fix 의 핵심 invariant 회귀 가드 다. parent
// deviceImpl 의 MigDevice(i) 호출 이 첫 호출 시 instance 를 cache 슬롯 에 저장 하고, 동일 index 의 후속
// 호출 은 동일 핸들 을 그대로 반환 해야 한다. 본 invariant 가 깨지면 instance handle 의 processUtilLastSeenTs
// 와 unsupported 캐시 가 매 poll 마다 초기화 되어 sample 중복 / NOT_SUPPORTED 반복 호출 결함이 재발한다.
func TestDeviceImpl_MigDeviceCacheReturnsSameHandle(t *testing.T) {
	d := &deviceImpl{}
	// cache 에 fake instance 를 직접 주입 해 NVML 호출 없이 cache hit 분기 만 검증.
	fakeMig := &deviceImpl{
		info: types.GPUDevice{UUID: "MIG-test", MigMode: types.MigModeEnabled},
	}
	d.migInstancesCache = map[int]*deviceImpl{0: fakeMig}

	first, err := d.MigDevice(0)
	if err != nil {
		t.Fatalf("MigDevice(0) err=%v", err)
	}
	if first != fakeMig {
		t.Fatal("MigDevice(0) 가 cache 핸들 외 다른 값 반환")
	}

	second, err := d.MigDevice(0)
	if err != nil {
		t.Fatalf("MigDevice(0) 재호출 err=%v", err)
	}
	if first != second {
		t.Error("MigDevice(0) 재호출 결과 가 첫 호출 과 다름. cache invariant 깨짐")
	}
}

// TestDeviceImpl_MigDeviceCacheNilSlotPreservesEmpty 는 빈 슬롯 (instance 미생성 또는 disabled) 의 nil
// 캐시가 후속 호출 에서도 (nil, nil) 로 정상 반환 되어 매 poll 빈 슬롯 재조회 비용 이 발생 하지 않음을 검증.
func TestDeviceImpl_MigDeviceCacheNilSlotPreservesEmpty(t *testing.T) {
	d := &deviceImpl{}
	d.migInstancesCache = map[int]*deviceImpl{2: nil}

	got, err := d.MigDevice(2)
	if err != nil {
		t.Fatalf("MigDevice(2) err=%v", err)
	}
	if got != nil {
		t.Error("빈 슬롯 캐시 가 nil 외 다른 값 반환")
	}
}

// TestDeviceImpl_CloseCascadesToMigChildren 는 #104 G1 fix 의 lifecycle 책임 invariant 회귀 가드 다.
// parent deviceImpl 의 Close 가 migInstancesCache 의 자식 인스턴스 들에 Close 를 cascade 호출 하고
// 캐시 슬롯을 nil 로 비움 으로써 stale handle 재사용 차단 과 자원 해제 일관성 을 보장 해야 한다.
func TestDeviceImpl_CloseCascadesToMigChildren(t *testing.T) {
	d := &deviceImpl{}
	child1 := &deviceImpl{info: types.GPUDevice{UUID: "MIG-1"}}
	child2 := &deviceImpl{info: types.GPUDevice{UUID: "MIG-2"}}
	d.migInstancesCache = map[int]*deviceImpl{0: child1, 1: child2, 2: nil}

	if err := d.Close(); err != nil {
		t.Fatalf("Close err=%v", err)
	}

	if d.migInstancesCache != nil {
		t.Error("Close 후 migInstancesCache 가 nil 로 정리 되지 않음")
	}
}
