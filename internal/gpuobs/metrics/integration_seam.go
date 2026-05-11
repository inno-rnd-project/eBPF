//go:build integration

package metrics

import (
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// 본 파일은 통합 테스트 (internal/gpuobs/integration) 가 metrics 패키지의 누적 시리즈 상태를
// 검증 / 초기화할 수 있게 노출하는 export-only 진입점을 모은다. //go:build integration 빌드 태그로
// 보호되어 production 바이너리 / 일반 단위 테스트 빌드에는 포함되지 않는다.

// ResetCudaStateForTest 는 cuda 카운터 / 심볼 가용성 / 누락 카운터 / multi-GPU 검출 게이지 / seenCudaKeys
// 추적기를 모두 초기화한다. 통합 테스트가 동일 process 안에서 누적 시리즈를 공유하지 않도록
// 매 테스트 진입 시점에 호출한다.
func ResetCudaStateForTest() {
	cudaKernelLaunchesTotal.Reset()
	cudaH2DBytesTotal.Reset()
	cudaD2HBytesTotal.Reset()
	cudaDtoDBytesTotal.Reset()
	cudaUnknownDirBytesTotal.Reset()
	cudaSymbolAvailable.Reset()
	cudaEventsLostTotal.Reset()
	cudaPidMultiGPUCount.Reset()
	seenCudaKeysMu.Lock()
	seenCudaKeys = make(map[CudaLabelKey]struct{})
	seenCudaKeysMu.Unlock()
}

// GetCudaEventsLostForTest 는 노드별 events_lost_total 누적값을 반환한다.
func GetCudaEventsLostForTest(node string) float64 {
	return testutil.ToFloat64(cudaEventsLostTotal.WithLabelValues(node))
}

// GetCudaPidMultiGPUCountForTest 는 노드별 pid_multi_gpu_count gauge 의 현재 값을 반환한다.
func GetCudaPidMultiGPUCountForTest(node string) float64 {
	return testutil.ToFloat64(cudaPidMultiGPUCount.WithLabelValues(node))
}

// CountDeviceMetricSeriesForTest 는 device 단위 gauge 가 노출하는 시리즈 수를 반환한다.
// GPU_METRICS_ENABLED=false 토글 검증에 사용된다.
func CountDeviceMetricSeriesForTest() int {
	return testutil.CollectAndCount(deviceUtilization) +
		testutil.CollectAndCount(deviceMemoryUsed) +
		testutil.CollectAndCount(deviceMemoryTotal)
}

// CountPodMetricSeriesForTest 는 podMemoryUsed gauge 의 시리즈 수를 반환한다.
// GPUOBS_POD_METRICS_ENABLED=false 토글 검증에 사용된다.
func CountPodMetricSeriesForTest() int {
	return testutil.CollectAndCount(podMemoryUsed)
}

// CountCudaCounterSeriesForTest 는 cuda 카운터 5종이 발행한 시리즈 합계를 반환한다.
// CUDA uprobe 비활성 시점에 0 이 유지되어야 한다.
func CountCudaCounterSeriesForTest() int {
	return testutil.CollectAndCount(cudaKernelLaunchesTotal) +
		testutil.CollectAndCount(cudaH2DBytesTotal) +
		testutil.CollectAndCount(cudaD2HBytesTotal) +
		testutil.CollectAndCount(cudaDtoDBytesTotal) +
		testutil.CollectAndCount(cudaUnknownDirBytesTotal)
}

// ResetDeviceMetricsForTest 는 device 단위 gauge / counter 시리즈를 초기화한다. 테스트 격리를 위해
// 토글 검증 전에 호출한다.
func ResetDeviceMetricsForTest() {
	deviceUtilization.Reset()
	deviceMemoryUsed.Reset()
	deviceMemoryTotal.Reset()
}

// ResetPodMetricsForTest 는 podMemoryUsed gauge 를 초기화한다.
func ResetPodMetricsForTest() {
	podMemoryUsed.Reset()
}
