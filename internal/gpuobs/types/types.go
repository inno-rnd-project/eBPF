package types

// GPUDevice는 관측 대상 GPU 한 개의 정적 식별 정보를 담는다.
// 모든 필드는 device 수명 동안 불변이며 nvml 계층의 Device 생성자에서 1회 채운 뒤 캐싱한다.
// 정적 특성(Compute capability / Architecture / 최대 PCIe 스펙 / CUDA core 수 / 메모리 버스 폭 /
// VBIOS·GSP firmware 버전) 은 metrics 계층에서 info gauge 라벨로 노출되어 fleet-wide grouping에 사용된다.
type GPUDevice struct {
	Index uint
	UUID  string
	Model string

	// CudaCompute*는 GetCudaComputeCapability 결과(major.minor). 0/0이면 NVML 호출 실패로 미수집된 상태.
	CudaComputeMajor int
	CudaComputeMinor int

	// Architecture는 GetArchitecture 결과를 사람이 읽을 수 있는 소문자 이름("ampere"/"ada"/"hopper"/"blackwell" 등) 으로 매핑한 값이다.
	// 매핑 불가능한 enum이면 "unknown"으로 채운다. 라벨 값으로 그대로 노출되므로 케이스 일관성을 유지한다.
	Architecture string

	// MaxPcieLink* 는 GetMaxPcieLinkGeneration / GetMaxPcieLinkWidth 결과. 0이면 미수집.
	MaxPcieLinkGeneration int
	MaxPcieLinkWidth      int

	// NumGpuCores는 GetNumGpuCores 결과 (CUDA core 수). 0이면 미수집.
	NumGpuCores int

	// MemoryBusWidthBits는 GetMemoryBusWidth 결과 (bits). 0이면 미수집.
	MemoryBusWidthBits uint32

	// VbiosVersion / GspFirmwareVersion은 펌웨어 회귀 디버깅용 버전 문자열. 빈 문자열이면 미수집.
	VbiosVersion       string
	GspFirmwareVersion string
}

// GPUSnapshot은 특정 시점에 측정된 GPU 상태를 담는다.
// 일부 필드는 GPU 모델에 따라 NVML이 NOT_SUPPORTED를 반환할 수 있어, 그런 경우 nvml 계층이
// `*Supported` 플래그를 false로 채워 metrics 계층에서 발행을 건너뛸 수 있게 한다.
type GPUSnapshot struct {
	Device           GPUDevice
	UtilizationPct   uint
	MemoryUsedBytes  uint64
	MemoryTotalBytes uint64
	TemperatureC     uint
	PowerUsageWatts  float64

	// MemoryCopyUtilPct는 NVML GetUtilizationRates의 Memory copy 엔진 사용률 (0-100) 이다.
	// 동일 호출에서 GPU util과 함께 반환되므로 추가 NVML 호출 비용은 없다.
	MemoryCopyUtilPct uint

	// PcieRxBps / PcieTxBps는 NVML이 20ms 윈도우로 sample한 PCIe RX/TX 대역폭이다 (bytes/sec).
	// NVML은 KB/s로 보고하므로 nvml 계층에서 ×1024 변환해 채운다. 미지원 시 PcieSupported=false.
	PcieRxBps     uint64
	PcieTxBps     uint64
	PcieSupported bool

	// ThrottleReasons는 GetCurrentClocksThrottleReasons가 반환한 bitmask다.
	// metrics 계층에서 known reason 비트별로 분해해 `reason` 라벨로 발행한다. 미지원 시 false.
	ThrottleReasons          uint64
	ThrottleReasonsSupported bool

	// ClockMhz 시리즈는 GetClockInfo로 도메인별 현재 clock을 조회한 값이다.
	// 도메인 단위로 미지원 가능하므로 *Supported 플래그를 분리해 둔다.
	ClockSMMhz             uint32
	ClockSMSupported       bool
	ClockMemoryMhz         uint32
	ClockMemorySupported   bool
	ClockGraphicsMhz       uint32
	ClockGraphicsSupported bool

	// EccCorrectedTotal / EccUncorrectedTotal은 GetTotalEccErrors(VOLATILE_ECC) 절대값이다.
	// metrics 계층은 이 절대값에서 직전 poll 값을 빼 Counter.Add(delta)에 사용한다.
	// ECC 미지원 GPU(컨슈머 카드 등)에서는 EccSupported=false.
	EccCorrectedTotal   uint64
	EccUncorrectedTotal uint64
	EccSupported        bool

	// EncoderUtilPct / DecoderUtilPct는 NVENC/NVDEC 엔진 사용률 (0-100) 이다.
	// GetEncoderUtilization / GetDecoderUtilization이 (util, samplingPeriodUs)를 함께 반환하지만
	// 본 프로젝트는 sampling period에 의존하지 않아 util만 보관한다.
	EncoderUtilPct   uint
	EncoderSupported bool
	DecoderUtilPct   uint
	DecoderSupported bool

	// PerformanceState는 NVML Pstates 값(0=최고 성능, 15=idle, 32=unknown)을 그대로 보관한다.
	// uint8 범위로 충분 (NVML enum 값이 32 이하).
	PerformanceState          uint8
	PerformanceStateSupported bool

	// FanSpeedPct는 GetFanSpeed가 반환하는 fan duty cycle (0-100) 이다.
	// passive cooling 카드(데이터센터 GPU 다수)는 NOT_SUPPORTED를 돌려주므로 *Supported 게이트가 필요하다.
	FanSpeedPct       uint
	FanSpeedSupported bool

	// BAR1MemoryUsed/Total는 GetBAR1MemoryInfo가 보고하는 PCIe BAR1 메모리(호스트가 직접 매핑하는 영역)의
	// 사용량 / 총량 (bytes). MIG/vGPU 환경에서 NOT_SUPPORTED 가능성이 있어 BAR1Supported로 게이트한다.
	BAR1MemoryUsedBytes  uint64
	BAR1MemoryTotalBytes uint64
	BAR1Supported        bool

	// PowerLimitWatts는 GetPowerManagementLimit가 보고하는 현재 적용된 power limit (Watts).
	// NVML이 milliwatts로 반환하므로 nvml 계층에서 1000으로 나눠 채운다.
	// 미지원 GPU에서는 PowerLimitSupported=false.
	PowerLimitWatts     float64
	PowerLimitSupported bool

	// ViolationTimesNs는 GetViolationStatus가 PerfPolicyType별로 반환한 누적 throttle 시간 (nanoseconds, since boot).
	// 키는 reason 라벨 문자열("power", "thermal", ...)이고 값은 NVML 절대 누적값이다.
	// metrics 계층은 ECC와 동일하게 직전 poll 값을 빼 양수 delta만 Counter.Add에 사용한다.
	// 일부 reason만 지원되는 카드도 있어 fill 단계에서 reason별 NOT_SUPPORTED를 흡수하고,
	// 적어도 하나라도 성공하면 ViolationSupported=true로 설정한다.
	ViolationTimesNs   map[string]uint64
	ViolationSupported bool

	// GPM* 시리즈는 NVML GPM (GPU Performance Monitoring) API가 두 sample 사이의 평균 사용률을 % 단위로 반환한 값이다.
	// 데이터센터 GPU(H100/A100 등) 전용 기능이라 컨슈머 카드(RTX 3090 등)에서는 GpmQueryDeviceSupport가
	// IsSupportedDevice=0을 반환한다. 그 경우 GpmSupported=false로 채워 metrics 계층에서 발행을 건너뛴다.
	// 또한 GPM은 두 sample이 모여야 첫 metric 산출이 가능하므로 첫 poll에서는 GpmFirstSampleReady=false 상태로
	// 값 발행을 건너뛴다.
	GpmGraphicsUtilPct  float64
	GpmSMOccupancyPct   float64
	GpmTensorActivePct  float64
	GpmDramBandwidthPct float64
	GpmSupported        bool
	GpmFirstSampleReady bool

	// EnergyConsumptionMilliJoules는 GetTotalEnergyConsumption이 반환한 누적 에너지 (mJ, since driver load).
	// metrics 계층은 ECC와 동일한 baseline-then-delta 패턴으로 처리하되 발행 시점에 mJ → J 환산해 Counter.Add에 사용한다.
	EnergyConsumptionMilliJoules uint64
	EnergySupported              bool

	// PcieLink* Current는 현재 PCIe 링크의 동적 상태로, idle 시 다운(예: gen4 x16 → gen1 x4) 되었다가 활성 시 복귀한다.
	// PCIe rx/tx bps 메트릭의 해석 컨텍스트로 함께 본다. gen / width 한 쪽만 미지원되는 카드는 사실상 없어 단일 *Supported로 게이트.
	PcieLinkGenerationCurrent uint32
	PcieLinkWidthCurrent      uint32
	PcieLinkSupported         bool

	// PcieReplayErrors는 GetPcieReplayCounter가 반환한 누적 PCIe 링크 재전송 횟수.
	// 값이 빠르게 증가하면 라이저 / 케이블 / 슬롯 문제 시그널이라 baseline-then-delta로 Counter 발행한다.
	PcieReplayErrors    uint32
	PcieReplaySupported bool

	// TemperatureThresholdsCelsius는 GetTemperatureThreshold(slowdown/shutdown/mem_max/gpu_max) 결과를 reason 키로 보관한다.
	// 값은 device 수명 동안 불변이라 nvml 계층에서 1회 fetch + 캐싱하고 매 poll에서 동일 map을 그대로 채운다.
	// 일부 threshold만 지원되는 GPU도 있어 키 단위 graceful skip 한다 — 적어도 하나라도 성공하면 *Supported=true.
	TemperatureThresholdsCelsius map[string]uint32
	TemperatureThresholdSupported bool

	// PowerLimitEnforcedWatts는 GetEnforcedPowerLimit 결과 (Watts). 정상 환경에서는 PowerLimitWatts와 동일하지만
	// driver-level capping(예: thermal/load 기반 자동 하향) 시 차이가 발생해 진단 단서가 된다.
	PowerLimitEnforcedWatts     float64
	PowerLimitEnforcedSupported bool

	// PersistenceModeEnabled는 nvidia-persistenced 활성 여부 (1=enabled, 0=disabled).
	// disabled 환경은 첫 CUDA context 생성 시 driver init 비용이 커 cold-start가 느려진다.
	PersistenceModeEnabled   uint8
	PersistenceModeSupported bool

	// ComputeMode는 NVML compute mode (0=Default, 1=ExclusiveThread, 2=Prohibited, 3=ExclusiveProcess).
	// 멀티 워크로드 환경에서 의도치 않게 ExclusiveProcess로 설정된 카드를 진단하기 위한 지표.
	ComputeMode          uint8
	ComputeModeSupported bool
}

// GPUProcess는 특정 GPU에서 실행 중인 프로세스 단위 기록이다.
// Phase 3에서 PID를 Pod로 귀속시키는 단계의 입력이 된다.
type GPUProcess struct {
	DeviceIndex     uint
	PID             uint32
	MemoryUsedBytes uint64
}
