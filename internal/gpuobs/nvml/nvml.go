// Package nvml은 NVIDIA Management Library 호출을 gpuobs 전용 인터페이스로 추상화하고,
// go-nvml(libnvidia-ml.so.1 dlopen) 기반 구현을 함께 제공한다. 인터페이스를 분리해둔
// 덕에 테스트는 fake NVML을 주입해 non-GPU 환경에서도 collector 동작을 검증할 수 있다.
package nvml

import (
	"fmt"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"

	"netobs/internal/gpuobs/types"
)

// NVML은 NVIDIA Management Library 호출을 추상화한 인터페이스다.
type NVML interface {
	DeviceCount() (uint, error)
	Device(index uint) (Device, error)
	Shutdown() error
}

// Device는 개별 GPU device에 대한 읽기 전용 접근을 제공한다.
type Device interface {
	Info() (types.GPUDevice, error)
	Snapshot() (types.GPUSnapshot, error)
	RunningProcesses() ([]types.GPUProcess, error)
}

// Init은 NVML 라이브러리(libnvidia-ml.so.1)를 런타임에 dlopen하고 초기화한다.
// 실패 시 에러를 반환하므로 호출자(collector)가 warn 로그 후 graceful disable을
// 선택할 수 있으며, non-GPU 노드에서도 바이너리가 기동을 멈추지 않는다.
func Init() (NVML, error) {
	if err := nvmlErr("nvml init", gonvml.Init()); err != nil {
		return nil, err
	}
	return &nvmlImpl{}, nil
}

// nvmlErr은 NVML 반환값을 Go 에러로 래핑하며 SUCCESS는 nil을 반환한다.
func nvmlErr(op string, ret gonvml.Return) error {
	if ret == gonvml.SUCCESS {
		return nil
	}
	return fmt.Errorf("%s: %s", op, gonvml.ErrorString(ret))
}

type nvmlImpl struct{}

func (n *nvmlImpl) DeviceCount() (uint, error) {
	count, ret := gonvml.DeviceGetCount()
	if err := nvmlErr("device count", ret); err != nil {
		return 0, err
	}
	return uint(count), nil
}

func (n *nvmlImpl) Device(index uint) (Device, error) {
	handle, ret := gonvml.DeviceGetHandleByIndex(int(index))
	if err := nvmlErr(fmt.Sprintf("device handle idx=%d", index), ret); err != nil {
		return nil, err
	}

	d := &deviceImpl{handle: handle, index: index}

	// UUID와 모델명은 device 수명 동안 불변이므로 최초 1회 조회해 `info`에 캐싱한다.
	// 이후 Snapshot은 NVML 재조회 없이 캐시된 값을 그대로 재사용한다.
	uuid, ret := handle.GetUUID()
	if err := d.wrapErr("device uuid", ret); err != nil {
		return nil, err
	}
	name, ret := handle.GetName()
	if err := d.wrapErr("device name", ret); err != nil {
		return nil, err
	}
	d.info = types.GPUDevice{Index: index, UUID: uuid, Model: name}

	return d, nil
}

func (n *nvmlImpl) Shutdown() error {
	return nvmlErr("nvml shutdown", gonvml.Shutdown())
}

type deviceImpl struct {
	handle gonvml.Device
	index  uint
	info   types.GPUDevice
}

// wrapErr는 device 호출 에러에 device index 컨텍스트를 덧붙여 래핑한다.
func (d *deviceImpl) wrapErr(op string, ret gonvml.Return) error {
	if ret == gonvml.SUCCESS {
		return nil
	}
	return fmt.Errorf("%s idx=%d: %s", op, d.index, gonvml.ErrorString(ret))
}

// Info는 Device 생성 시 캐싱된 정적 정보를 그대로 반환한다.
// 현재 구현에서는 에러를 돌려줄 경로가 없지만, 다른 백엔드에서의 재조회 패턴을 허용하기 위해
// 인터페이스 시그니처는 그대로 유지한다.
func (d *deviceImpl) Info() (types.GPUDevice, error) {
	return d.info, nil
}

func (d *deviceImpl) Snapshot() (types.GPUSnapshot, error) {
	util, ret := d.handle.GetUtilizationRates()
	if err := d.wrapErr("utilization", ret); err != nil {
		return types.GPUSnapshot{}, err
	}

	mem, ret := d.handle.GetMemoryInfo()
	if err := d.wrapErr("memory info", ret); err != nil {
		return types.GPUSnapshot{}, err
	}

	temp, ret := d.handle.GetTemperature(gonvml.TEMPERATURE_GPU)
	if err := d.wrapErr("temperature", ret); err != nil {
		return types.GPUSnapshot{}, err
	}

	// NVML power reporting 단위는 milliwatts이므로 1000으로 나눠 Watts로 변환한다.
	powerMilliWatts, ret := d.handle.GetPowerUsage()
	if err := d.wrapErr("power usage", ret); err != nil {
		return types.GPUSnapshot{}, err
	}

	return types.GPUSnapshot{
		Device:           d.info,
		UtilizationPct:   uint(util.Gpu),
		MemoryUsedBytes:  mem.Used,
		MemoryTotalBytes: mem.Total,
		TemperatureC:     uint(temp),
		PowerUsageWatts:  float64(powerMilliWatts) / 1000.0,
	}, nil
}

func (d *deviceImpl) RunningProcesses() ([]types.GPUProcess, error) {
	procs, ret := d.handle.GetComputeRunningProcesses()
	if err := d.wrapErr("running processes", ret); err != nil {
		return nil, err
	}
	result := make([]types.GPUProcess, 0, len(procs))
	for _, p := range procs {
		result = append(result, types.GPUProcess{
			DeviceIndex:     d.index,
			PID:             p.Pid,
			MemoryUsedBytes: p.UsedGpuMemory,
		})
	}
	return result, nil
}
