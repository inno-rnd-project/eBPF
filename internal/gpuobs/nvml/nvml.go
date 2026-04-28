// Package nvml은 NVIDIA Management Library 호출을 gpuobs 전용 인터페이스로 추상화하고,
// go-nvml(libnvidia-ml.so.1 dlopen) 기반 구현을 함께 제공한다. 인터페이스를 분리해둔
// 덕에 테스트는 fake NVML을 주입해 non-GPU 환경에서도 collector 동작을 검증할 수 있다.
package nvml

import (
	"fmt"
	"log"
	"sync"

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
// 실패 시 에러를 반환하며, 상위 초기화 경로(현재 `cmd/gpuobs-agent/main.go`)가 이를 기록한 뒤
// collector에 nil 핸들을 주입해 graceful disable을 구성한다.
// 그 결과 non-GPU 노드에서도 바이너리가 기동을 멈추지 않는다.
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

	d := &deviceImpl{handle: handle, index: index, unsupported: make(map[string]struct{})}

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

	// unsupported는 NOT_SUPPORTED를 한 번이라도 반환한 metric의 키 셋이다.
	// 다음 Snapshot부터 해당 metric은 NVML 호출을 건너뛰어 컨슈머 GPU 등에서 매 poll마다 발생하던
	// NOT_SUPPORTED 로그/호출 비용을 제거한다. 키는 metric 식별 문자열(예: "pcie", "ecc").
	// Snapshot은 단일 goroutine(collector pollOnce)에서만 호출되지만 Device 인터페이스가
	// public이라 향후 동시 호출 가능성을 차단하기 위해 mu로 보호한다 (read 우세 → RWMutex).
	mu          sync.RWMutex
	unsupported map[string]struct{}
}

// wrapErr는 device 호출 에러에 device index 컨텍스트를 덧붙여 래핑한다.
func (d *deviceImpl) wrapErr(op string, ret gonvml.Return) error {
	if ret == gonvml.SUCCESS {
		return nil
	}
	return fmt.Errorf("%s idx=%d: %s", op, d.index, gonvml.ErrorString(ret))
}

// markUnsupported는 metric 키를 unsupported 셋에 추가하고 1회 warn 로그를 남긴다.
// 같은 키가 반복되면 한 번만 로그 처리해 출력 스팸을 회피한다.
// 쓰기 작업이라 mu.Lock으로 보호한다.
func (d *deviceImpl) markUnsupported(metric string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, seen := d.unsupported[metric]; seen {
		return
	}
	d.unsupported[metric] = struct{}{}
	log.Printf("gpuobs: nvml metric %q not supported on device idx=%d (%s); skipping in subsequent polls",
		metric, d.index, d.info.Model)
}

// isUnsupported는 해당 metric이 이전에 NOT_SUPPORTED로 표시되었는지 검사한다.
// 매 poll마다 모든 metric 진입에서 호출되어 호출 빈도가 높아 RLock으로 read 동시성을 허용한다.
func (d *deviceImpl) isUnsupported(metric string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	_, ok := d.unsupported[metric]
	return ok
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

	snap := types.GPUSnapshot{
		Device:            d.info,
		UtilizationPct:    uint(util.Gpu),
		MemoryCopyUtilPct: uint(util.Memory),
		MemoryUsedBytes:   mem.Used,
		MemoryTotalBytes:  mem.Total,
		TemperatureC:      uint(temp),
		PowerUsageWatts:   float64(powerMilliWatts) / 1000.0,
	}

	d.fillPcieThroughput(&snap)
	d.fillThrottleReasons(&snap)
	d.fillClocks(&snap)
	d.fillEccErrors(&snap)
	d.fillEncoderDecoder(&snap)
	d.fillPerformanceState(&snap)

	return snap, nil
}

// fillPcieThroughput은 RX/TX 두 카운터를 한 번에 채운다. 둘 중 하나가 NOT_SUPPORTED면 양쪽 모두 비활성으로 간주한다.
// NVML이 KB/s 단위 sample을 반환하므로 1024 곱해 bytes/sec로 정규화한다.
func (d *deviceImpl) fillPcieThroughput(snap *types.GPUSnapshot) {
	if d.isUnsupported("pcie") {
		return
	}
	rx, ret := d.handle.GetPcieThroughput(gonvml.PCIE_UTIL_RX_BYTES)
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("pcie")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: pcie rx idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	tx, ret := d.handle.GetPcieThroughput(gonvml.PCIE_UTIL_TX_BYTES)
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("pcie")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: pcie tx idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	snap.PcieRxBps = uint64(rx) * 1024
	snap.PcieTxBps = uint64(tx) * 1024
	snap.PcieSupported = true
}

func (d *deviceImpl) fillThrottleReasons(snap *types.GPUSnapshot) {
	if d.isUnsupported("throttle") {
		return
	}
	reasons, ret := d.handle.GetCurrentClocksThrottleReasons()
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("throttle")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: throttle reasons idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	snap.ThrottleReasons = uint64(reasons)
	snap.ThrottleReasonsSupported = true
}

// fillClocks는 SM/Memory/Graphics 도메인을 개별로 조회해 도메인별 *Supported를 분리 보고한다.
// 한 도메인이 NOT_SUPPORTED여도 다른 도메인은 정상 발행될 수 있다.
func (d *deviceImpl) fillClocks(snap *types.GPUSnapshot) {
	if mhz, ok := d.fetchClock("clock.sm", gonvml.CLOCK_SM); ok {
		snap.ClockSMMhz = mhz
		snap.ClockSMSupported = true
	}
	if mhz, ok := d.fetchClock("clock.memory", gonvml.CLOCK_MEM); ok {
		snap.ClockMemoryMhz = mhz
		snap.ClockMemorySupported = true
	}
	if mhz, ok := d.fetchClock("clock.graphics", gonvml.CLOCK_GRAPHICS); ok {
		snap.ClockGraphicsMhz = mhz
		snap.ClockGraphicsSupported = true
	}
}

func (d *deviceImpl) fetchClock(metric string, ct gonvml.ClockType) (uint32, bool) {
	if d.isUnsupported(metric) {
		return 0, false
	}
	mhz, ret := d.handle.GetClockInfo(ct)
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported(metric)
		return 0, false
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: %s idx=%d: %s", metric, d.index, gonvml.ErrorString(ret))
		return 0, false
	}
	return mhz, true
}

// fillEccErrors는 corrected/uncorrected 모두 한 번에 시도하고, 둘 중 하나라도 NOT_SUPPORTED면 ECC 전체를 비활성 표시한다.
// VOLATILE_ECC를 사용해 boot 이후 누적값을 가져온다 (재부팅 시 0 리셋).
func (d *deviceImpl) fillEccErrors(snap *types.GPUSnapshot) {
	if d.isUnsupported("ecc") {
		return
	}
	corrected, ret := d.handle.GetTotalEccErrors(gonvml.MEMORY_ERROR_TYPE_CORRECTED, gonvml.VOLATILE_ECC)
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("ecc")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: ecc corrected idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	uncorrected, ret := d.handle.GetTotalEccErrors(gonvml.MEMORY_ERROR_TYPE_UNCORRECTED, gonvml.VOLATILE_ECC)
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("ecc")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: ecc uncorrected idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	snap.EccCorrectedTotal = corrected
	snap.EccUncorrectedTotal = uncorrected
	snap.EccSupported = true
}

func (d *deviceImpl) fillEncoderDecoder(snap *types.GPUSnapshot) {
	if !d.isUnsupported("encoder") {
		encUtil, _, ret := d.handle.GetEncoderUtilization()
		if ret == gonvml.ERROR_NOT_SUPPORTED {
			d.markUnsupported("encoder")
		} else if ret != gonvml.SUCCESS {
			log.Printf("gpuobs: encoder util idx=%d: %s", d.index, gonvml.ErrorString(ret))
		} else {
			snap.EncoderUtilPct = uint(encUtil)
			snap.EncoderSupported = true
		}
	}
	if !d.isUnsupported("decoder") {
		decUtil, _, ret := d.handle.GetDecoderUtilization()
		if ret == gonvml.ERROR_NOT_SUPPORTED {
			d.markUnsupported("decoder")
		} else if ret != gonvml.SUCCESS {
			log.Printf("gpuobs: decoder util idx=%d: %s", d.index, gonvml.ErrorString(ret))
		} else {
			snap.DecoderUtilPct = uint(decUtil)
			snap.DecoderSupported = true
		}
	}
}

func (d *deviceImpl) fillPerformanceState(snap *types.GPUSnapshot) {
	if d.isUnsupported("pstate") {
		return
	}
	pstate, ret := d.handle.GetPerformanceState()
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("pstate")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: performance state idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	snap.PerformanceState = uint8(pstate)
	snap.PerformanceStateSupported = true
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
