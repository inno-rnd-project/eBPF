package nvml

import "netobs/internal/gpuobs/types"

// NVML은 NVIDIA Management Library 호출을 추상화한 인터페이스이다.
// Phase 2에서 go-nvml dlopen 기반 구현이 이 인터페이스를 만족시킨다.
type NVML interface {
	// DeviceCount는 사용 가능한 GPU device의 개수를 반환한다.
	DeviceCount() (uint, error)

	// Device는 주어진 index의 device 핸들을 반환한다.
	Device(index uint) (Device, error)

	// Shutdown은 NVML 라이브러리를 해제한다.
	Shutdown() error
}

// Device는 개별 GPU device에 대한 읽기 전용 접근을 제공한다.
type Device interface {
	// Info는 device의 정적 식별 정보를 반환한다.
	Info() (types.GPUDevice, error)

	// Snapshot은 현재 시점의 device 상태를 반환한다.
	Snapshot() (types.GPUSnapshot, error)

	// RunningProcesses는 이 device에서 실행 중인 프로세스 목록을 반환한다.
	// Phase 3의 Pod 귀속 단계에서 입력으로 사용된다.
	RunningProcesses() ([]types.GPUProcess, error)
}
