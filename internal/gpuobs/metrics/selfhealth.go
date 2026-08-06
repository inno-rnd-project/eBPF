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

	// bpfMapUtilizationRatio 는 cuda uprobe 모듈 BPF map 의 current entries / max entries 비율이다
	// (#413). netobs 측 netobs_bpf_map_utilization_ratio 와 동일 의미로, cuda_tid_device 같은 LRU
	// map 의 evict 로 인한 표본 누락 (kernel launch 의 device 귀속 유실 등) 이 무증상으로 진행되지
	// 않게 한다. 판정 alert 는 본 이슈 범위 밖이라 붙이지 않는다.
	bpfMapUtilizationRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_bpf_map_utilization_ratio",
			Help: "Current/max entry ratio of gpuobs cuda BPF maps (cuda_tid_device, cuctx_to_device, cuctx_create_args, sync_starts). Sustained values near 1.0 mean LRU eviction is discarding samples.",
		},
		[]string{"map"},
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

	// dcgmAvailable은 #123의 NVIDIA DCGM 통합 가용성 self-health gauge다. cmd/gpuobs-agent의
	// wire-up 흐름이 dcgm.Source.Available 결과를 본 게이지에 set한다. dev cluster의 RTX 3090
	// 환경에서는 build tag dcgm 비활성으로 noopSource만 wire-up되어 0 emit으로 graceful
	// degradation 식별 진입점이 된다. 데이터센터 GPU 환경에서는 실제 SDK 통합 후 1 emit으로
	// 전환된다.
	dcgmAvailable = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "gpuobs_dcgm_available",
			Help: "1 if the NVIDIA DCGM source is reachable (SDK linked or dcgm-exporter endpoint OK), else 0. Drives the visibility of DCGM-derived dominant cause slots (e.g. dcgm_pcie_replay). On dev cluster RTX 3090 with the DCGM build tag disabled this gauge emits 0 as a graceful degradation signal.",
		},
	)

	// ncclProfilerAvailable은 #123의 NCCL profiler 가용성 self-health gauge다. cmd/gpuobs-
	// agent의 wire-up 흐름이 nccl.Profiler.Available 결과를 본 게이지에 set한다. RTX 3090
	// 환경에서는 noopProfiler만 wire-up되어 0 emit으로 graceful degradation 식별 진입점이
	// 된다. 데이터센터 GPU 환경에서는 cuProfiler symbol 또는 NCCL callback attach 후 1 emit
	// 으로 전환된다.
	ncclProfilerAvailable = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "gpuobs_nccl_profiler_available",
			Help: "1 if the NCCL collective profiler is attached (cuProfiler symbol or NCCL callback bound), else 0. Drives the visibility of NCCL-derived dominant cause slots (e.g. nccl_collective_stall). On dev cluster RTX 3090 with the NCCL build tag disabled this gauge emits 0 as a graceful degradation signal.",
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

// SetBpfMapUtilization 은 map 라벨 별 포화 비율을 emit 한다. ratio 는 호출 측 (cuda refresher)
// 에서 current / max 로 산정을 마친 값이다.
func SetBpfMapUtilization(mapName string, ratio float64) {
	bpfMapUtilizationRatio.WithLabelValues(mapName).Set(ratio)
}

// SetInformerSyncLag 는 informer staleness 게이지를 갱신한다. netobs 측 동명 함수와 동일 의미.
func SetInformerSyncLag(seconds float64) {
	informerSyncLagSeconds.Set(seconds)
}

// SetDcgmAvailable은 #123의 dcgmAvailable 게이지를 갱신한다. cmd/gpuobs-agent의 wire-up 흐름
// 이 dcgm.Source.Available의 boolean 결과를 1 또는 0으로 변환해 본 함수에 전달한다.
func SetDcgmAvailable(active bool) {
	if active {
		dcgmAvailable.Set(1)
		return
	}
	dcgmAvailable.Set(0)
}

// SetNcclProfilerAvailable은 #123의 ncclProfilerAvailable 게이지를 갱신한다. cmd/gpuobs-agent
// 의 wire-up 흐름이 nccl.Profiler.Available의 boolean 결과를 1 또는 0으로 변환해 본 함수에
// 전달한다.
func SetNcclProfilerAvailable(active bool) {
	if active {
		ncclProfilerAvailable.Set(1)
		return
	}
	ncclProfilerAvailable.Set(0)
}
