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

	ObserveNvmlCall("Snapshot", 0.01, "NVML_ERROR_NOT_SUPPORTED")

	if got := testutil.ToFloat64(nvmlErrorsTotal.WithLabelValues("Snapshot", "NVML_ERROR_NOT_SUPPORTED")); got != 1 {
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

	ObserveNvmlCall("Device", 0.001, "NVML_ERROR_NOT_SUPPORTED")
	ObserveNvmlCall("Device", 0.001, "NVML_ERROR_NOT_SUPPORTED")
	ObserveNvmlCall("Device", 0.001, "NVML_ERROR_GPU_IS_LOST")

	if got := testutil.ToFloat64(nvmlErrorsTotal.WithLabelValues("Device", "NVML_ERROR_NOT_SUPPORTED")); got != 2 {
		t.Errorf("NOT_SUPPORTED=%v; want 2", got)
	}
	if got := testutil.ToFloat64(nvmlErrorsTotal.WithLabelValues("Device", "NVML_ERROR_GPU_IS_LOST")); got != 1 {
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

// TestGpuobsSelfHealthRegister 는 Register 가 self-health 3 종을 모두 등록하는지 회귀 가드한다.
func TestGpuobsSelfHealthRegister(t *testing.T) {
	resetSelfHealth()
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	ObserveNvmlCall("DeviceCount", 0.001, "")
	ObserveNvmlCall("Device", 0.001, "NVML_ERROR_NOT_SUPPORTED")
	SetInformerSyncLag(1.0)

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
