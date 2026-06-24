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

// Source는 DCGM 통합의 추상 인터페이스다. gpuobs는 DCGM hardware counter를 자체 re-export 하지
// 않고 dcgm-exporter가 노출하는 메트릭 (DCGM_FI_DEV_PCIE_REPLAY_COUNTER 등) 을 Prometheus가 직접
// 스크랩한다. 따라서 본 인터페이스는 dcgm-exporter 가용성 health check (Available) 와 리소스 정리
// (Close) 만 둔다. 메트릭 re-export 경로 (MetricForward) 는 Prometheus 직접 스크랩과 중복 double-hop
// 이라 #156 에서 제거했다. 기본 구현은 noopSource 다.
type Source interface {
	// Available은 dcgm-exporter endpoint가 정상 연결되어 있는지 돌려준다. gpuobs_dcgm_available
	// self-health gauge의 값 산출에 사용된다. noopSource는 항상 false를 돌려주어 dev cluster의
	// RTX 3090 환경에서 graceful degradation 식별 진입점이 된다.
	Available() bool

	// Close는 HTTP client의 리소스 정리 진입점이다. noopSource는 nil을 돌려준다.
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

func (*noopSource) Close() error {
	return nil
}
