// Package dcgm 은 NVIDIA DCGM (Data Center GPU Manager) 통합 의 인터페이스 skeleton 이다. RTX 3090
// 단일 GPU 환경 에서는 DCGM 의 일부 메트릭 (NVLink throughput 와 PCIe replay count 등) 검증 이
// 불가 하므로 본 패키지 는 인터페이스 정의 와 noop 기본 구현 만 둔다. 실제 DCGM SDK 통합 은
// build tag (//go:build dcgm) 분리 또는 runtime dlopen 으로 SDK 부재 환경 의 빌드 깨짐 을 회피
// 하며 데이터센터 GPU (A100 과 H100 등) 환경 확보 시점 에 별도 PR 로 도입 한다.
//
// 본 패키지 는 internal/gpuobs/nvml 과 동등 레벨 의 leaf 패키지 다. NVML Device 인터페이스 와
// 별개 의 책임 영역 (NVML 은 process 단위 utilization, DCGM 은 device 단위 hardware counter)
// 이라 별도 패키지 로 분리 한다.
package dcgm

import "time"

// Sample 은 DCGM 메트릭 의 단일 sample 표현 이다. Name 은 메트릭 식별자 (예: "dcgm_pcie_replay_count")
// 이고 Labels 는 device 와 gpu_uuid 등 cardinality 가드 라벨 이다. Value 는 raw 측정 값 이고
// Timestamp 는 sample 시각 이다.
type Sample struct {
	Name      string
	Labels    map[string]string
	Value     float64
	Timestamp time.Time
}

// Source 는 DCGM 메트릭 fetch 의 추상 인터페이스 다. production 구현 은 build tag dcgm 분리 한
// 파일 에서 NVIDIA DCGM SDK 또는 dcgm-exporter HTTP endpoint 를 호출 한다. 본 패키지 의 기본
// 구현 은 noopSource 라 모든 메서드 가 graceful empty 결과 를 돌려 준다.
type Source interface {
	// Available 은 DCGM SDK 또는 dcgm-exporter endpoint 가 정상 연결 되어 있는지 돌려 준다.
	// gpuobs_dcgm_available self-health gauge 의 값 산출 에 사용 된다. noopSource 는 항상 false
	// 를 돌려 주어 dev cluster 의 RTX 3090 환경 에서 graceful degradation 식별 진입점 이 된다.
	Available() bool

	// MetricForward 는 prefix 로 필터 한 DCGM 메트릭 sample 슬라이스 를 돌려 준다. fetch 실패
	// 또는 SDK 미통합 환경 에서는 빈 슬라이스 를 돌려 준다. prefix 가 빈 문자열 이면 모든 메트릭
	// 을 돌려 준다.
	MetricForward(prefix string) []Sample

	// Close 는 SDK handle 또는 HTTP client 의 리소스 정리 진입점 이다. noopSource 는 nil 을
	// 돌려 준다.
	Close() error
}

// noopSource 는 본 패키지 의 기본 구현 이다. DCGM SDK 가 부재 한 환경 또는 build tag dcgm 이
// 비활성 인 빌드 에서 사용 된다. 모든 메서드 가 graceful empty 결과 를 돌려 주어 cmd/gpuobs-
// agent 의 wire-up 흐름 이 호출 자체 로 panic 없이 진행 된다.
type noopSource struct{}

// NewNoop 은 noopSource 의 factory 다. 본 PR 의 cmd/gpuobs-agent/main.go 가 GPUOBS_DCGM_ENABLED
// env 가 false (기본값) 일 때 본 factory 로 인스턴스 를 생성 한다.
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
