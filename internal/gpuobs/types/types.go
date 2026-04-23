package types

// GPUDevice는 관측 대상 GPU 한 개의 정적 식별 정보를 담는다.
type GPUDevice struct {
	Index uint
	UUID  string
	Model string
}

// GPUSnapshot은 특정 시점에 측정된 GPU 상태를 담는다.
// Phase 1에서는 타입만 정의되며, Phase 2의 NVML collector에서 이 값을 채운다.
type GPUSnapshot struct {
	Device           GPUDevice
	UtilizationPct   uint
	MemoryUsedBytes  uint64
	MemoryTotalBytes uint64
	TemperatureC     uint
	PowerUsageWatts  float64
}

// GPUProcess는 특정 GPU에서 실행 중인 프로세스 단위 기록이다.
// Phase 3에서 PID를 Pod로 귀속시키는 단계의 입력이 된다.
type GPUProcess struct {
	DeviceIndex     uint
	PID             uint32
	MemoryUsedBytes uint64
}