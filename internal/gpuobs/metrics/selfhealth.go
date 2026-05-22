package metrics

import "github.com/prometheus/client_golang/prometheus"

// self-health 메트릭은 gpuobs agent 자체의 NVML wrapper 호출 latency 와 에러 분포를 노출한다.
// netobs 측 selfhealth.go 와 동일한 self-observe 카테고리이며, NVML driver issue 또는 GPU 일시
// 적 unresponsive 상황을 운영자가 가시화할 수 있게 한다.
//
// instrumentation 대상은 NVML 인터페이스 (DeviceCount, Device, DeviceUUID, Shutdown) 와 Device
// 인터페이스 (Info, Snapshot, RunningProcesses, Close) 의 외부 노출 API 8 종이다. Snapshot 내부
// 의 개별 NVML 호출 (GetUtilizationRates 등) 까지 instrument 하면 cardinality 가 폭증해 외부 진입
// 경계에 한정한다. 호출 빈도는 collector poll 사이클 (보통 30s) 에 device 수를 곱한 수준이라 hot
// path 가 아니다.
var (
	// nvmlCallDuration 은 NVML wrapper 호출 8 종의 wall-clock latency 분포다. buckets 는 NVML 일반
	// 호출 범위 (수 µs 에서 수 ms 사이) 에 맞춰 10µs 부터 약 0.33s 까지 16 단계 exponential 로 둔다.
	nvmlCallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gpuobs_nvml_call_duration_seconds",
			Help:    "Wall-clock duration of NVML wrapper calls (DeviceCount, Device, DeviceUUID, Shutdown, Info, Snapshot, RunningProcesses, Close). Buckets cover the typical NVML range from ~10us to ~0.33s. Use histogram_quantile(0.99, ...) to detect driver-induced stalls.",
			Buckets: prometheus.ExponentialBuckets(1e-5, 2, 16),
		},
		[]string{"call"},
	)

	// nvmlErrorsTotal 은 NVML 호출 실패 카운터다. call 라벨이 호출 함수명, error_code 라벨이
	// nvml.Return enum 의 string 표현 (ERROR_NOT_SUPPORTED 등) 으로 노출되어 운영자가 dashboard
	// 와 alert 에서 정수 enum 을 외우지 않아도 의미를 즉시 파악할 수 있다. cardinality 는 호출
	// 함수명 8 종 × NVML enum 유한 수라 폐쇄적이다.
	nvmlErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_nvml_errors_total",
			Help: "Cumulative NVML wrapper call errors by function name and NVML return code (string form from gonvml.Return.String(), e.g. ERROR_NOT_SUPPORTED). Sustained non-zero rate indicates driver issues or GPU unresponsiveness; use this for the GPUObsAgentNvmlErrorsHigh alert.",
		},
		[]string{"call", "error_code"},
	)

	// informerSyncLagSeconds 는 netobs 측 동명 게이지와 같은 의미이며, gpuobs agent 가 kube
	// Resolver 를 사용할 때 노출한다. PodMetricsEnabled=false 운영 모드에서는 emit 자체가 skip
	// 된다.
	informerSyncLagSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "gpuobs_informer_sync_lag_seconds",
			Help: "Seconds since the kube informer last received any watch event for Pod / Service / Node. Before the first event the gauge falls back to seconds since agent startup. Stale informer cache is detected by sustained values well above the resync period.",
		},
	)
)

// ObserveNvmlCall 은 NVML wrapper 진입에서 호출되어 duration 을 observe 하고 에러 시 errors_total
// 을 increment 한다. 호출 측은 보통 defer 문에서 본 함수를 호출한다.
func ObserveNvmlCall(call string, durationSeconds float64, errCode string) {
	nvmlCallDuration.WithLabelValues(call).Observe(durationSeconds)
	if errCode != "" {
		nvmlErrorsTotal.WithLabelValues(call, errCode).Inc()
	}
}

// SetInformerSyncLag 는 informer staleness 게이지를 갱신한다. netobs 측 동명 함수와 동일 의미.
func SetInformerSyncLag(seconds float64) {
	informerSyncLagSeconds.Set(seconds)
}
