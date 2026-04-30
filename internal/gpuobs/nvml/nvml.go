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
// Close는 device 수명 동안 알록된 자원(GPM sample 버퍼 등)을 해제한다.
// collector.Run이 NVML.Shutdown 직전에 모든 device에 대해 호출한다.
type Device interface {
	Info() (types.GPUDevice, error)
	Snapshot() (types.GPUSnapshot, error)
	RunningProcesses() ([]types.GPUProcess, error)
	Close() error
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

	d := &deviceImpl{handle: handle, index: index, unsupported: make(map[string]struct{}), gpmPreviousIdx: -1}

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

	// GPM 지원 여부는 device 수명 동안 불변이므로 1회 조회해 캐싱한다.
	// 미지원이면 fillGpm은 매 poll에서 즉시 early return하고, sample 버퍼 할당도 건너뛴다.
	d.initGpm()

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

	// GPM은 두 sample(현재/직전) 사이의 평균 사용률을 산출한다. sample 버퍼는 initGpm에서 1회 alloc하고
	// 이후 GpmSampleGet으로 재사용(write back)한다. gpmPreviousIdx는 직전 sample을 담은 슬롯 인덱스(0/1)로,
	// -1은 "아직 sample을 받은 적 없음"을 뜻한다.
	// gpmDeviceSupport=false면 fillGpm은 매 poll에서 즉시 return하며 sample 버퍼는 할당되지 않는다.
	// 본 필드들은 Snapshot 단일 goroutine 계약 아래서만 변경되며, Close는 collector ctx 종료 후 호출되어
	// Snapshot과 동시 실행되지 않는다. 따라서 별도 mutex는 두지 않는다.
	gpmDeviceSupport bool
	gpmAllocated     bool
	gpmSamples       [2]gonvml.GpmSample
	gpmPreviousIdx   int
}

// violationReasons는 GetViolationStatus를 호출할 PerfPolicyType 8종이다. 각 reason은 NVML이
// 별도 호출로 노출하므로 device당 매 poll에서 최대 8회 NVML 호출이 추가된다(첫 NOT_SUPPORTED 응답 이후
// 해당 reason은 markUnsupported로 캐시되어 다음 poll부터 호출 건너뜀).
// reason 라벨은 metrics 계층의 deviceViolationLabels와 1:1 대응되며, 새 reason을 추가하려면 양쪽을 함께 수정한다.
var violationReasons = []struct {
	name   string
	policy gonvml.PerfPolicyType
}{
	{"power", gonvml.PERF_POLICY_POWER},
	{"thermal", gonvml.PERF_POLICY_THERMAL},
	{"sync_boost", gonvml.PERF_POLICY_SYNC_BOOST},
	{"board_limit", gonvml.PERF_POLICY_BOARD_LIMIT},
	{"low_utilization", gonvml.PERF_POLICY_LOW_UTILIZATION},
	{"reliability", gonvml.PERF_POLICY_RELIABILITY},
	{"total_app_clocks", gonvml.PERF_POLICY_TOTAL_APP_CLOCKS},
	{"total_base_clocks", gonvml.PERF_POLICY_TOTAL_BASE_CLOCKS},
}

// gpmMetricIDs는 본 PR이 수집하는 GPM metric ID 4종이다. snap 필드와 1:1로 대응시키는 순서를 유지한다.
// 추가 GPM ID를 받으려면 본 슬라이스와 fillGpm 내 매핑, types.GPUSnapshot 필드를 함께 수정한다.
var gpmMetricIDs = [4]gonvml.GpmMetricId{
	gonvml.GPM_METRIC_GRAPHICS_UTIL,
	gonvml.GPM_METRIC_SM_OCCUPANCY,
	gonvml.GPM_METRIC_ANY_TENSOR_UTIL,
	gonvml.GPM_METRIC_DRAM_BW_UTIL,
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
	d.fillFanSpeed(&snap)
	d.fillBAR1Memory(&snap)
	d.fillPowerLimit(&snap)
	d.fillViolationStatus(&snap)
	d.fillGpm(&snap)

	return snap, nil
}

// fillPcieThroughput은 RX/TX 두 카운터를 한 번에 채운다. NVML 자체는 두 호출을 분리해 노출하지만
// PCIe 인터페이스의 양방향이라 한쪽만 지원되는 GPU는 사실상 존재하지 않는다. 한 쪽이 NOT_SUPPORTED면
// "PCIe 자체 미지원"으로 간주해 양쪽 모두 비활성 표시하며, 그 결과 markUnsupported("pcie") 1회 호출로
// 다음 poll부터 두 호출 모두 건너뛴다. NVML이 KB/s 단위 sample을 반환하므로 1024 곱해 bytes/sec로 정규화한다.
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

func (d *deviceImpl) fillFanSpeed(snap *types.GPUSnapshot) {
	if d.isUnsupported("fan") {
		return
	}
	speed, ret := d.handle.GetFanSpeed()
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("fan")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: fan speed idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	snap.FanSpeedPct = uint(speed)
	snap.FanSpeedSupported = true
}

func (d *deviceImpl) fillBAR1Memory(snap *types.GPUSnapshot) {
	if d.isUnsupported("bar1") {
		return
	}
	bar1, ret := d.handle.GetBAR1MemoryInfo()
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("bar1")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: bar1 memory idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	snap.BAR1MemoryUsedBytes = bar1.Bar1Used
	snap.BAR1MemoryTotalBytes = bar1.Bar1Total
	snap.BAR1Supported = true
}

// fillPowerLimit은 현재 적용된 power management limit (Watts)을 채운다. NVML은 milliwatts로 반환하므로 1000으로 나눈다.
// EnforcedPowerLimit 대신 PowerManagementLimit을 사용해 "운영자가 nvidia-smi -pl로 설정한 값"을 그대로 노출한다.
func (d *deviceImpl) fillPowerLimit(snap *types.GPUSnapshot) {
	if d.isUnsupported("power_limit") {
		return
	}
	limitMilliWatts, ret := d.handle.GetPowerManagementLimit()
	if ret == gonvml.ERROR_NOT_SUPPORTED {
		d.markUnsupported("power_limit")
		return
	}
	if ret != gonvml.SUCCESS {
		log.Printf("gpuobs: power limit idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}
	snap.PowerLimitWatts = float64(limitMilliWatts) / 1000.0
	snap.PowerLimitSupported = true
}

// fillViolationStatus는 reason별 누적 throttle 시간을 모두 조회해 map에 채운다.
// reason 한 종이 NOT_SUPPORTED여도 다른 reason은 유지될 수 있어 markUnsupported는 reason별 키로 분리한다.
// 적어도 하나라도 성공하면 ViolationSupported=true로 설정해 metrics 계층이 발행 모드로 진입하게 한다.
// 모든 reason이 미지원이거나 호출 실패만 있는 경우 map 자체를 alloc하지 않아 graceful skip 카드의 alloc 비용을 제거한다.
func (d *deviceImpl) fillViolationStatus(snap *types.GPUSnapshot) {
	var times map[string]uint64
	for _, r := range violationReasons {
		key := "violation." + r.name
		if d.isUnsupported(key) {
			continue
		}
		v, ret := d.handle.GetViolationStatus(r.policy)
		if ret == gonvml.ERROR_NOT_SUPPORTED {
			d.markUnsupported(key)
			continue
		}
		if ret != gonvml.SUCCESS {
			log.Printf("gpuobs: violation %s idx=%d: %s", r.name, d.index, gonvml.ErrorString(ret))
			continue
		}
		if times == nil {
			times = make(map[string]uint64, len(violationReasons))
		}
		times[r.name] = v.ViolationTime
	}
	if len(times) > 0 {
		snap.ViolationTimesNs = times
		snap.ViolationSupported = true
	}
}

// initGpm은 device 생성 시 1회 호출되어 GPM 지원 여부를 캐싱하고 sample 버퍼 두 개를 미리 alloc한다.
// 미지원이면 alloc을 건너뛰어 RTX 3090 같은 컨슈머 카드에서 매 poll alloc/free 비용을 제거한다.
// 본 함수는 Device가 collector에 노출되기 전에 호출되므로 Snapshot과 동시 실행되지 않는다.
func (d *deviceImpl) initGpm() {
	support, ret := d.handle.GpmQueryDeviceSupport()
	if ret != gonvml.SUCCESS {
		// 원인을 분리한다:
		//   - ERROR_NOT_SUPPORTED: GPU 자체가 GPM 미지원 (대부분의 컨슈머 카드). markUnsupported가 1회 warn 로그를 남겨 운영자가 즉시 인지하게 한다.
		//   - 그 외 (ERROR_FUNCTION_NOT_FOUND, ERROR_ARGUMENT_VERSION_MISMATCH 등): 드라이버 / API 버전 불일치 등으로 GPM "지원 여부 자체를 단정할 수 없는" 상태이므로
		//     "not supported" warn 로그로 운영자를 오도하지 않고 ret 코드를 그대로 남긴다. 어떤 경로든 gpmDeviceSupport=false가 유지되어 fillGpm은 매 poll silent skip한다.
		if ret == gonvml.ERROR_NOT_SUPPORTED {
			d.markUnsupported("gpm")
		} else {
			log.Printf("gpuobs: gpm support query idx=%d failed: ret=%d (%s); GPM remains uninitialized", d.index, ret, gonvml.ErrorString(ret))
		}
		return
	}
	if support.IsSupportedDevice == 0 {
		d.markUnsupported("gpm")
		return
	}

	// 두 sample 버퍼를 미리 alloc해 매 poll 호출 시 GpmSampleGet이 재사용하도록 한다.
	for i := 0; i < 2; i++ {
		s, ret := gonvml.GpmSampleAlloc()
		if ret != gonvml.SUCCESS {
			// alloc 실패는 호스트 메모리 부족 등 일시적 자원 문제일 수 있어 "미지원"으로 영구 캐시하지 않는다.
			// 영구 unsupported 캐시 대신 ret 코드를 보존해 운영자가 원인을 추적할 수 있게 한다.
			// 결과적으로 gpmAllocated=false / gpmDeviceSupport=false 상태가 유지되어 fillGpm/Close 모두 nop이며,
			// agent 재기동 시 다시 init을 시도할 수 있다.
			log.Printf("gpuobs: gpm sample alloc failed idx=%d slot=%d ret=%d (%s); GPM remains uninitialized", d.index, i, ret, gonvml.ErrorString(ret))
			// 일부 alloc 성공 후 실패한 경우 부분 free + 슬롯 nil 클리어 (use-after-free 방어).
			for j := 0; j < i; j++ {
				_ = d.gpmSamples[j].Free()
				d.gpmSamples[j] = nil
			}
			return
		}
		d.gpmSamples[i] = s
	}
	d.gpmAllocated = true
	d.gpmDeviceSupport = true
}

// fillGpm은 ping-pong 슬롯에 sample을 받고 두 sample이 모이면 GpmMetricsGet으로 평균 사용률을 산출한다.
// 첫 호출에서는 GpmFirstSampleReady=false로 채워 metrics 계층이 발행을 건너뛴다.
func (d *deviceImpl) fillGpm(snap *types.GPUSnapshot) {
	if !d.gpmDeviceSupport || !d.gpmAllocated {
		return
	}

	// 다음 sample이 들어갈 슬롯. 첫 호출(previousIdx=-1)이면 0번 슬롯 사용.
	nextIdx := 0
	if d.gpmPreviousIdx == 0 {
		nextIdx = 1
	}
	curr := d.gpmSamples[nextIdx]
	if ret := curr.Get(d.handle); ret != gonvml.SUCCESS {
		log.Printf("gpuobs: gpm sample get idx=%d: %s", d.index, gonvml.ErrorString(ret))
		return
	}

	// 첫 sample은 baseline일 뿐 metric 계산은 건너뛴다.
	if d.gpmPreviousIdx < 0 {
		d.gpmPreviousIdx = nextIdx
		snap.GpmSupported = true
		// GpmFirstSampleReady=false 그대로 두면 metrics 계층이 발행을 건너뛴다.
		return
	}

	prev := d.gpmSamples[d.gpmPreviousIdx]
	get := &gonvml.GpmMetricsGetType{
		NumMetrics: uint32(len(gpmMetricIDs)),
		Sample1:    prev,
		Sample2:    curr,
	}
	for i, id := range gpmMetricIDs {
		get.Metrics[i].MetricId = uint32(id)
	}
	if ret := gonvml.GpmMetricsGet(get); ret != gonvml.SUCCESS {
		log.Printf("gpuobs: gpm metrics get idx=%d: %s", d.index, gonvml.ErrorString(ret))
		// metrics 호출 자체가 실패해도 다음 poll에서 재시도. previousIdx는 새 sample을 가리키도록 갱신한다.
		d.gpmPreviousIdx = nextIdx
		return
	}

	// gpmMetricIDs와 1:1 대응 순서로 snapshot에 채운다. NvmlReturn이 SUCCESS인 metric만 신뢰한다.
	for i, m := range get.Metrics[:len(gpmMetricIDs)] {
		if gonvml.Return(m.NvmlReturn) != gonvml.SUCCESS {
			continue
		}
		switch gpmMetricIDs[i] {
		case gonvml.GPM_METRIC_GRAPHICS_UTIL:
			snap.GpmGraphicsUtilPct = m.Value
		case gonvml.GPM_METRIC_SM_OCCUPANCY:
			snap.GpmSMOccupancyPct = m.Value
		case gonvml.GPM_METRIC_ANY_TENSOR_UTIL:
			snap.GpmTensorActivePct = m.Value
		case gonvml.GPM_METRIC_DRAM_BW_UTIL:
			snap.GpmDramBandwidthPct = m.Value
		}
	}
	snap.GpmSupported = true
	snap.GpmFirstSampleReady = true
	d.gpmPreviousIdx = nextIdx
}

// Close는 device가 alloc한 GPM sample 버퍼를 해제한다. collector.Run이 ctx 취소 후 NVML.Shutdown 직전에 호출한다.
// 본 함수가 호출되는 시점에는 Snapshot이 더 이상 호출되지 않음을 collector가 보장한다.
// 호출이 두 번 들어오더라도 두 번째는 gpmAllocated=false 가드로 nop.
func (d *deviceImpl) Close() error {
	if !d.gpmAllocated {
		return nil
	}
	for i := range d.gpmSamples {
		if ret := d.gpmSamples[i].Free(); ret != gonvml.SUCCESS {
			log.Printf("gpuobs: gpm sample free idx=%d slot=%d: %s", d.index, i, gonvml.ErrorString(ret))
		}
	}
	// stale handle 재사용을 차단하기 위해 슬롯을 비운다 (use-after-free 방어).
	d.gpmSamples = [2]gonvml.GpmSample{}
	d.gpmAllocated = false
	return nil
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
