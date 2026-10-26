package nvml

import (
	"testing"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/NVIDIA/go-nvml/pkg/nvml/mock"

	"netobs/internal/gpuobs/types"
)

// newCoreMock은 core 4 호출의 반환 코드를 주입한 go-nvml mock device와 이를 감싼 deviceImpl을 만든다.
func newCoreMock(util, mem, temp, power gonvml.Return) (*deviceImpl, *mock.Device) {
	m := &mock.Device{
		GetUtilizationRatesFunc: func() (gonvml.Utilization, gonvml.Return) {
			return gonvml.Utilization{Gpu: 70, Memory: 20}, util
		},
		GetMemoryInfoFunc: func() (gonvml.Memory, gonvml.Return) {
			return gonvml.Memory{Used: 1 << 30, Total: 24 << 30}, mem
		},
		GetTemperatureFunc: func(gonvml.TemperatureSensors) (uint32, gonvml.Return) {
			return 65, temp
		},
		GetPowerUsageFunc: func() (uint32, gonvml.Return) {
			return 250000, power
		},
	}
	d := &deviceImpl{handle: m, info: types.GPUDevice{Model: "A100"}, unsupported: map[string]struct{}{}}
	return d, m
}

// TestFillCore_MigParentUtilizationUnsupported는 #457의 핵심 회귀 가드다. MIG 활성 parent가
// GetUtilizationRates에 NOT_SUPPORTED를 돌려줘도 Snapshot이 실패하지 않고, utilization만 결측으로 두며
// 나머지 core 지표는 채워져야 한다. 종전에는 이 경우 device 전체가 버려졌다.
func TestFillCore_MigParentUtilizationUnsupported(t *testing.T) {
	d, m := newCoreMock(gonvml.ERROR_NOT_SUPPORTED, gonvml.SUCCESS, gonvml.SUCCESS, gonvml.SUCCESS)
	var snap types.GPUSnapshot

	ret, err := d.fillCore(&snap)
	if err != nil || ret != gonvml.SUCCESS {
		t.Fatalf("fillCore ret=%v err=%v, want SUCCESS/nil (NOT_SUPPORTED 흡수)", ret, err)
	}
	if snap.UtilizationSupported {
		t.Errorf("UtilizationSupported=true, want false")
	}
	if !snap.MemorySupported || snap.MemoryTotalBytes != 24<<30 {
		t.Errorf("memory=%v/%d, want 지원·24GiB", snap.MemorySupported, snap.MemoryTotalBytes)
	}
	if !snap.TemperatureSupported || snap.TemperatureC != 65 {
		t.Errorf("temperature=%v/%d, want 지원·65", snap.TemperatureSupported, snap.TemperatureC)
	}
	if !snap.PowerUsageSupported || snap.PowerUsageWatts != 250 {
		t.Errorf("power=%v/%v, want 지원·250W", snap.PowerUsageSupported, snap.PowerUsageWatts)
	}

	// 미지원 캐시로 다음 poll부터 utilization 호출 자체를 건너뛴다 (fill* 계열과 동일 규약).
	var next types.GPUSnapshot
	if _, err := d.fillCore(&next); err != nil {
		t.Fatalf("두 번째 fillCore err=%v", err)
	}
	if got := len(m.GetUtilizationRatesCalls()); got != 1 {
		t.Errorf("GetUtilizationRates 호출=%d, want 1 (미지원 캐시 후 생략)", got)
	}
	if got := len(m.GetMemoryInfoCalls()); got != 2 {
		t.Errorf("GetMemoryInfo 호출=%d, want 2", got)
	}
}

// TestFillCore_RealFailureStillFails는 NOT_SUPPORTED가 아닌 실제 장애는 종전대로 Snapshot 실패로
// 전파되어 device가 건너뛰어지는지, 그리고 반환 ret이 errors_total 계수용 실패 코드인지 검증한다.
func TestFillCore_RealFailureStillFails(t *testing.T) {
	d, _ := newCoreMock(gonvml.SUCCESS, gonvml.SUCCESS, gonvml.ERROR_GPU_IS_LOST, gonvml.SUCCESS)
	var snap types.GPUSnapshot

	ret, err := d.fillCore(&snap)
	if err == nil {
		t.Fatalf("fillCore err=nil, want GPU_IS_LOST 전파")
	}
	if ret != gonvml.ERROR_GPU_IS_LOST {
		t.Errorf("ret=%v, want ERROR_GPU_IS_LOST", ret)
	}
	if d.isUnsupported("temperature") {
		t.Errorf("실제 장애가 미지원 캐시로 오분류됨")
	}
}

// TestFillCore_VgpuTemperaturePowerUnsupported는 온도와 전력을 지원하지 않는 vGPU 프로파일 형태에서
// 두 필드만 결측이고 utilization과 memory는 채워지는지 검증한다.
func TestFillCore_VgpuTemperaturePowerUnsupported(t *testing.T) {
	d, _ := newCoreMock(gonvml.SUCCESS, gonvml.SUCCESS, gonvml.ERROR_NOT_SUPPORTED, gonvml.ERROR_NOT_SUPPORTED)
	var snap types.GPUSnapshot

	if _, err := d.fillCore(&snap); err != nil {
		t.Fatalf("fillCore err=%v", err)
	}
	if !snap.UtilizationSupported || snap.UtilizationPct != 70 || snap.MemoryCopyUtilPct != 20 {
		t.Errorf("utilization=%v/%d/%d, want 지원·70·20", snap.UtilizationSupported, snap.UtilizationPct, snap.MemoryCopyUtilPct)
	}
	if snap.TemperatureSupported || snap.PowerUsageSupported {
		t.Errorf("temperature/power 지원=%v/%v, want 둘 다 false", snap.TemperatureSupported, snap.PowerUsageSupported)
	}
}
