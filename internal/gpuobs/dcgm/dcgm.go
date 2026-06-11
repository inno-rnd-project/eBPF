// Package dcgm은 NVIDIA DCGM (Data Center GPU Manager) 통합의 인터페이스 skeleton이다. RTX 3090
// 단일 GPU 환경에서는 DCGM의 일부 메트릭 (NVLink throughput와 PCIe replay count 등) 검증이
// 불가하므로 본 패키지는 인터페이스 정의와 noop 기본 구현만 둔다. 실제 DCGM SDK 통합은
// build tag (//go:build dcgm) 분리 또는 runtime dlopen으로 SDK 부재 환경의 빌드 깨짐을 회피
// 하며 데이터센터 GPU (A100과 H100 등) 환경 확보 시점에 별도 PR로 도입한다.
//
// 본 패키지는 internal/gpuobs/nvml과 동등 레벨의 leaf 패키지다. NVML Device 인터페이스와
// 별개의 책임 영역 (NVML은 process 단위 utilization, DCGM은 device 단위 hardware counter)
// 이라 별도 패키지로 분리한다.
package dcgm

import "time"

// Sample은 DCGM 메트릭의 단일 sample 표현이다. Name은 메트릭 식별자 (예: "dcgm_pcie_replay_count")
// 이고 Labels는 device와 gpu_uuid 등 cardinality 가드 라벨이다. Value는 raw 측정 값이고
// Timestamp는 sample 시각이다.
type Sample struct {
	Name      string
	Labels    map[string]string
	Value     float64
	Timestamp time.Time
}

// Source는 DCGM 메트릭 fetch의 추상 인터페이스다. production 구현은 build tag dcgm 분리한
// 파일에서 NVIDIA DCGM SDK 또는 dcgm-exporter HTTP endpoint를 호출한다. 본 패키지의 기본
// 구현은 noopSource라 모든 메서드가 graceful empty 결과를 돌려준다.
type Source interface {
	// Available은 DCGM SDK 또는 dcgm-exporter endpoint가 정상 연결되어 있는지 돌려준다.
	// gpuobs_dcgm_available self-health gauge의 값 산출에 사용된다. noopSource는 항상 false
	// 를 돌려주어 dev cluster의 RTX 3090 환경에서 graceful degradation 식별 진입점이 된다.
	Available() bool

	// MetricForward는 prefix로 필터한 DCGM 메트릭 sample 슬라이스를 돌려준다. fetch 실패
	// 또는 SDK 미통합 환경에서는 빈 슬라이스를 돌려준다. prefix가 빈 문자열이면 모든 메트릭
	// 을 돌려준다.
	MetricForward(prefix string) []Sample

	// Close는 SDK handle 또는 HTTP client의 리소스 정리 진입점이다. noopSource는 nil을
	// 돌려준다.
	Close() error
}

// noopSource는 본 패키지의 기본 구현이다. DCGM SDK가 부재한 환경 또는 build tag dcgm이
// 비활성인 빌드에서 사용된다. 모든 메서드가 graceful empty 결과를 돌려주어 cmd/gpuobs-
// agent의 wire-up 흐름이 호출 자체로 panic 없이 진행된다.
type noopSource struct{}

// NewNoop은 noopSource의 factory다. 본 PR의 cmd/gpuobs-agent/main.go가 GPUOBS_DCGM_ENABLED
// env가 false (기본값) 일 때 본 factory로 인스턴스를 생성한다.
func NewNoop() Source {
	return &noopSource{}
}

func (*noopSource) Available() bool {
	return false
}

func (*noopSource) MetricForward(prefix string) []Sample {
	return nil
}

func (*noopSource) Close() error {
	return nil
}
