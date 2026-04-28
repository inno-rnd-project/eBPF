package types

// GPUDevice는 관측 대상 GPU 한 개의 정적 식별 정보를 담는다.
type GPUDevice struct {
	Index uint
	UUID  string
	Model string
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
}

// GPUProcess는 특정 GPU에서 실행 중인 프로세스 단위 기록이다.
// Phase 3에서 PID를 Pod로 귀속시키는 단계의 입력이 된다.
type GPUProcess struct {
	DeviceIndex     uint
	PID             uint32
	MemoryUsedBytes uint64
}
