package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// resetSelfHealth 는 selfhealth.go 의 패키지 전역 상태를 test 사이에 격리한다. selfhealth 측 메트릭
// 만 다루므로 metrics.go 의 큰 reset 헬퍼와 분리한다.
func resetSelfHealth() {
	nvmlCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "gpuobs_nvml_call_duration_seconds", Buckets: prometheus.ExponentialBuckets(1e-5, 2, 16)},
		[]string{"call"},
	)
	nvmlErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "gpuobs_nvml_errors_total"},
		[]string{"call", "error_code"},
	)
	informerSyncLagSeconds = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gpuobs_informer_sync_lag_seconds"})
	dcgmAvailable = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gpuobs_dcgm_available"})
	ncclProfilerAvailable = prometheus.NewGauge(prometheus.GaugeOpts{Name: "gpuobs_nccl_profiler_available"})
}

// TestSetDcgmAvailable은 #123의 dcgmAvailable 게이지 setter가 boolean 입력을 1과 0으로 정확히
// 변환하는지 회귀 가드한다. dev cluster의 RTX 3090 환경 default인 false 분기와 데이터센터
// GPU 환경의 true 분기 모두 검증한다.
func TestSetDcgmAvailable(t *testing.T) {
	resetSelfHealth()

	SetDcgmAvailable(false)
	if got := testutil.ToFloat64(dcgmAvailable); got != 0 {
		t.Errorf("dcgmAvailable=%v want 0 after SetDcgmAvailable(false)", got)
	}
	SetDcgmAvailable(true)
	if got := testutil.ToFloat64(dcgmAvailable); got != 1 {
		t.Errorf("dcgmAvailable=%v want 1 after SetDcgmAvailable(true)", got)
	}
}

// TestSetNcclProfilerAvailable은 #123의 ncclProfilerAvailable 게이지 setter가 boolean 입력을
// 1과 0으로 정확히 변환하는지 회귀 가드한다.
func TestSetNcclProfilerAvailable(t *testing.T) {
	resetSelfHealth()

	SetNcclProfilerAvailable(false)
	if got := testutil.ToFloat64(ncclProfilerAvailable); got != 0 {
		t.Errorf("ncclProfilerAvailable=%v want 0 after SetNcclProfilerAvailable(false)", got)
	}
	SetNcclProfilerAvailable(true)
	if got := testutil.ToFloat64(ncclProfilerAvailable); got != 1 {
		t.Errorf("ncclProfilerAvailable=%v want 1 after SetNcclProfilerAvailable(true)", got)
	}
}

// TestObserveNvmlCall_SuccessOmitsErrorEmit 은 SUCCESS 케이스에서 duration 만 observe 되고
// errors_total emit 이 발생하지 않는지 검증한다. error_code 가 빈 문자열일 때 cardinality 가
// 라벨 폭증 없이 통제되는 회귀 가드다.
func TestObserveNvmlCall_SuccessOmitsErrorEmit(t *testing.T) {
	resetSelfHealth()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(nvmlCallDuration, nvmlErrorsTotal)

	ObserveNvmlCall("DeviceCount", 0.002, "")

	if got := testutil.CollectAndCount(nvmlCallDuration); got != 1 {
		t.Errorf("histogram series=%d; want 1", got)
	}
	if got := testutil.CollectAndCount(nvmlErrorsTotal); got != 0 {
		t.Errorf("errors series=%d; want 0 (success must not emit errors)", got)
	}
}

// TestObserveNvmlCall_ErrorEmitsBothMetrics 는 NVML 에러 케이스에서 duration 과 errors_total 양쪽
// 이 emit 되는지 검증한다.
func TestObserveNvmlCall_ErrorEmitsBothMetrics(t *testing.T) {
	resetSelfHealth()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(nvmlCallDuration, nvmlErrorsTotal)

	ObserveNvmlCall("Snapshot", 0.01, "ERROR_NOT_SUPPORTED")

	if got := testutil.ToFloat64(nvmlErrorsTotal.WithLabelValues("Snapshot", "ERROR_NOT_SUPPORTED")); got != 1 {
		t.Errorf("errors counter=%v; want 1", got)
	}
	if got := testutil.CollectAndCount(nvmlCallDuration); got != 1 {
		t.Errorf("histogram series=%d; want 1", got)
	}
}

// TestObserveNvmlCall_LabelCardinalityClosed 는 같은 call 의 여러 error_code 가 각각 별도 시리즈
// 로 emit 되되 동일 (call, error_code) 페어는 누적되는지 검증한다.
func TestObserveNvmlCall_LabelCardinalityClosed(t *testing.T) {
	resetSelfHealth()

	ObserveNvmlCall("Device", 0.001, "ERROR_NOT_SUPPORTED")
	ObserveNvmlCall("Device", 0.001, "ERROR_NOT_SUPPORTED")
	ObserveNvmlCall("Device", 0.001, "ERROR_GPU_IS_LOST")

	if got := testutil.ToFloat64(nvmlErrorsTotal.WithLabelValues("Device", "ERROR_NOT_SUPPORTED")); got != 2 {
		t.Errorf("NOT_SUPPORTED=%v; want 2", got)
	}
	if got := testutil.ToFloat64(nvmlErrorsTotal.WithLabelValues("Device", "ERROR_GPU_IS_LOST")); got != 1 {
		t.Errorf("GPU_IS_LOST=%v; want 1", got)
	}
}

// TestSetInformerSyncLag_GpuobsOverwrite 는 gpuobs 측 informer_sync_lag gauge 가 호출마다 최신
// 값으로 덮어쓰기 되는지 검증한다. netobs 측 동명 함수와 의미가 일치한다.
func TestSetInformerSyncLag_GpuobsOverwrite(t *testing.T) {
	resetSelfHealth()

	SetInformerSyncLag(10.0)
	SetInformerSyncLag(2.5)

	if got := testutil.ToFloat64(informerSyncLagSeconds); got != 2.5 {
		t.Errorf("overwrite=%v; want 2.5", got)
	}
}

// TestGpuobsSelfHealthRegister는 Register가 self-health 5종을 모두 등록하는지 회귀 가드한다.
// #123의 dcgmAvailable과 ncclProfilerAvailable 추가 후 등록 정합 검증 셋을 5종으로 확장한다.
func TestGpuobsSelfHealthRegister(t *testing.T) {
	resetSelfHealth()
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	ObserveNvmlCall("DeviceCount", 0.001, "")
	ObserveNvmlCall("Device", 0.001, "ERROR_NOT_SUPPORTED")
	SetInformerSyncLag(1.0)
	SetDcgmAvailable(false)
	SetNcclProfilerAvailable(false)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	want := []string{
		"gpuobs_nvml_call_duration_seconds",
		"gpuobs_nvml_errors_total",
		"gpuobs_informer_sync_lag_seconds",
		"gpuobs_dcgm_available",
		"gpuobs_nccl_profiler_available",
	}
	for _, n := range want {
		if !names[n] {
			t.Errorf("metric %q missing after Register; have %s", n, strings.Join(mapKeys(names), ", "))
		}
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
