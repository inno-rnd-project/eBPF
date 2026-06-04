// Package metrics는 gpuobs 에이전트가 Prometheus로 발행하는 지표를 정의한다.
// gpuobs 전용 프리픽스 `gpuobs_`를 써서 netobs 지표(`netobs_*`)와 네임스페이스를 분리한다.
package metrics

import (
	"strconv"
	"strings"
	"sync"

	gonvml "github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// AgentVersion은 에이전트의 버전 문자열이며, Phase 4 릴리스에서 ldflags로 치환된다.
// Phase 1에서는 "dev" 고정 문자열을 쓴다.
const AgentVersion = "dev"

var agentInfo = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name:        "gpuobs_agent_info",
		Help:        "Static information about the gpuobs agent, value is always 1",
		ConstLabels: prometheus.Labels{"version": AgentVersion},
	},
)

// deviceLabels는 device 단위 gauge의 공통 라벨 세트다. UUID는 안정적 식별자,
// index는 slot, model은 그래프 범주화, node는 클러스터 내 귀속에 쓴다.
var deviceLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model"}

// migModeLabels 는 #104 의 gpuobs_mig_mode self-health 라벨 세트다. mode 라벨 은 unsupported / disabled
// / enabled 3종 이며 각 device 마다 3 시리즈 모두 0 또는 1 로 발행 해 운영자 가 단일 query
// (`gpuobs_mig_mode{mode="enabled"} == 1`) 로 환경별 분기 가시화 가 가능 하다.
var migModeLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "mode"}

// mpsActiveLabels 는 #104 의 gpuobs_mps_active self-health 라벨 세트다. MPS daemon 활성 여부 는 노드
// 단위 신호 지만 dashboard 정합 (다른 device 메트릭 과 동일 키 join) 을 위해 device 라벨 4종 그대로 노출.
var mpsActiveLabels = deviceLabels

// deviceClockLabels는 도메인별 clock gauge에 `clock` 라벨을 추가한 변형이다.
var deviceClockLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "clock"}

// deviceThrottleLabels는 활성 throttle 사유를 `reason` 라벨로 분해한 변형이다.
var deviceThrottleLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "reason"}

// deviceEccLabels는 corrected/uncorrected를 `error_type` 라벨로 분해한 counter용 라벨 세트다.
var deviceEccLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "error_type"}

// deviceViolationLabels는 PerfPolicyType별 누적 throttle 시간을 `reason` 라벨로 분해한 counter용 라벨 세트다.
// reason 값은 nvml 계층 violationReasons 슬라이스와 정확히 일치한다.
var deviceViolationLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "reason"}

// deviceGpmLabels는 GPM 메트릭 4종을 `gpm_metric` 라벨로 분해한 gauge용 라벨 세트다.
// 4종을 별도 시리즈로 분리하면 운영자가 PromQL `sum by(gpm_metric)`으로 grouping할 수 있다.
var deviceGpmLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "gpm_metric"}

// deviceTemperatureThresholdLabels는 4종 threshold(slowdown/shutdown/mem_max/gpu_max) 를 `threshold` 라벨로 분해한 gauge용 라벨 세트다.
var deviceTemperatureThresholdLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "threshold"}

// deviceInfoLabels는 정적 device 특성(compute capability / architecture / 최대 PCIe 스펙 / CUDA core 수 / 메모리 버스 폭) 을
// 라벨로 노출하는 info gauge용 라벨 세트다. value는 항상 1이며 라벨 값으로 fleet-wide grouping에 사용된다.
var deviceInfoLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "compute_capability", "architecture", "max_pcie_generation", "max_pcie_width", "num_cores", "memory_bus_width_bits"}

// deviceFirmwareInfoLabels는 펌웨어 회귀 디버깅용 라벨 세트다. value는 항상 1.
var deviceFirmwareInfoLabels = []string{"node", "gpu_uuid", "gpu_index", "gpu_model", "vbios_version", "gsp_firmware_version"}

// throttleReasonBits는 NVML이 보고하는 known throttle 사유 9종이다. 매 poll마다 9개를 모두
// 0/1로 발행해 "직전엔 있었는데 이번엔 사라진" 라벨이 stale로 남는 일을 회피한다.
// 동일 비트값을 갖는 alias(ApplicationsClocksSetting/UserDefinedClocks=2)는 한 번만 노출한다.
// 비트값을 raw 정수가 아닌 go-nvml 명명 상수로 두어 NVML 헤더 변경 시 컴파일 단계에서 잡히도록 한다.
var throttleReasonBits = []struct {
	name string
	bit  uint64
}{
	{"gpu_idle", uint64(gonvml.ClocksThrottleReasonGpuIdle)},
	{"applications_clocks_setting", uint64(gonvml.ClocksThrottleReasonApplicationsClocksSetting)},
	{"sw_power_cap", uint64(gonvml.ClocksThrottleReasonSwPowerCap)},
	{"hw_slowdown", uint64(gonvml.ClocksThrottleReasonHwSlowdown)},
	{"sync_boost", uint64(gonvml.ClocksThrottleReasonSyncBoost)},
	{"sw_thermal_slowdown", uint64(gonvml.ClocksThrottleReasonSwThermalSlowdown)},
	{"hw_thermal_slowdown", uint64(gonvml.ClocksThrottleReasonHwThermalSlowdown)},
	{"hw_power_brake_slowdown", uint64(gonvml.ClocksThrottleReasonHwPowerBrakeSlowdown)},
	{"display_clock_setting", uint64(gonvml.ClocksThrottleReasonDisplayClockSetting)},
}

// lastEccAbsolute는 device(UUID)별 error_type별 직전 poll의 NVML 절대값을 보관한다.
// VOLATILE_ECC가 노드 부팅 이후 누적값을 반환하므로 delta = current - prev 로 Counter.Add에 쓴다.
// current < prev (드라이버 리셋 등) 시에는 데이터 연속성 단절로 간주해 가산을 건너뛰고 새 baseline으로만 갱신한다 — 거짓 spike 회피.
// 호출자는 단일 goroutine(collector pollOnce)이지만 본 변수가 패키지 전역이고 Record가
// public 함수라 향후 호출 패턴 변경에 대비해 lastEccAbsoluteMu로 보호한다.
var (
	lastEccAbsolute   = make(map[string]uint64)
	lastEccAbsoluteMu sync.Mutex
)

// lastViolationAbsolute는 device(UUID)별 reason별 직전 poll의 NVML 누적 ns 값을 보관한다.
// ECC와 동일한 baseline-then-delta 패턴이지만 단위는 nanoseconds이고, Counter.Add 시 1e9로 나눠 seconds로 환산한다.
// 첫 poll은 baseline만 저장(Add 건너뜀)해 "에이전트 기동 이후 신규 throttle 시간"이라는 counter 의미를 보존한다.
// current < prev (드라이버 리셋 등) 시에는 가산을 건너뛰고 새 baseline으로만 갱신해 dashboard 거짓 spike를 회피한다.
var (
	lastViolationAbsolute   = make(map[string]uint64)
	lastViolationAbsoluteMu sync.Mutex
)

// lastEnergyAbsolute는 device(UUID)별 NVML 누적 에너지 (mJ since driver load) 직전 poll 값을 보관한다.
// ECC/Violation과 동일한 baseline-then-delta 패턴이며 발행 시점에 mJ → J 환산한다.
// driver reload 등으로 current < prev 일 때는 가산을 건너뛰고 새 baseline 으로만 갱신한다.
var (
	lastEnergyAbsolute   = make(map[string]uint64)
	lastEnergyAbsoluteMu sync.Mutex
)

// lastPcieReplayAbsolute는 device(UUID)별 PCIe replay counter 직전 poll 값을 보관한다.
// NVML이 uint32로 반환하는 누적값이며 baseline-then-delta로 처리한다.
// uint32 wrap-around 또는 driver reload 시 current < prev 가 발생할 수 있으며 이 경우 가산을 건너뛰고 새 baseline 만 갱신한다.
var (
	lastPcieReplayAbsolute   = make(map[string]uint32)
	lastPcieReplayAbsoluteMu sync.Mutex
)

// podLabels는 per-pod gauge의 공통 라벨 세트다. 앞 4개(node/src_namespace/src_pod/src_pod_uid)는
// netobs `netobs_pod_stage_events_labeled_total`과 정확히 일치해 PromQL 조인 키로 쓰일 수 있다.
// gpu_uuid/gpu_index는 GPU 차원을 추가해 한 Pod이 복수 GPU를 사용하는 경우 분리 측정한다.
var podLabels = []string{"node", "src_namespace", "src_pod", "src_pod_uid", "gpu_uuid", "gpu_index"}

// cudaPodLabels 는 cuda uprobe 카운터가 공유하는 Pod 차원 라벨 세트다.
// 앞 4개는 netobs/podLabels 와 동일한 4-key 조인 표준이고, 마지막 gpu_uuid 는 NVML
// GetComputeRunningProcesses 캐시로 PID 를 GPU 에 매핑한 결과다. gpu_index 는 cuda
// 메트릭에 포함하지 않아 라벨 카디널리티를 1축 줄이며 PromQL 조인 시 gpu_index 가
// 필요하면 podMemoryUsed 시리즈와 결합해 보강할 수 있다.
var cudaPodLabels = []string{"node", "src_namespace", "src_pod", "src_pod_uid", "gpu_uuid"}

// cudaSymbolLabels 는 libcuda / libcudart 심볼 상태 gauge 의 라벨 세트다.
// 노드 × 추적 심볼 (libcuda 14 + libcudart 3 = 17종) = 노드당 최대 17 시리즈.
// libcudart 3종은 GPUOBS_CUDA_LIBCUDART_PATH 가 비어 있으면 attach 시도 자체를 건너뛰지만
// availability=0 시리즈는 동일하게 발행되어 운영자가 cudart 비활성 상태를 메트릭으로 식별할 수 있게 한다.
var cudaSymbolLabels = []string{"node", "symbol"}

// podMetricsEnabled는 per-pod gauge(`gpuobs_pod_*`) 기록 여부를 결정한다.
// 클러스터 규모가 클 때 src_pod / src_pod_uid 라벨로 인한 Prometheus 카디널리티 폭증을
// 막기 위한 escape hatch로, 기본값은 true(기록)다. SetPodMetricsEnabled로 startup 시점에만
// 갱신되고 그 이후에는 읽기 전용으로 쓴다.
var podMetricsEnabled = true

// SetPodMetricsEnabled는 per-pod 지표 기록 여부를 전환하며 반드시 RecordPodSnapshot 호출 전(main startup)에만 호출되어야 한다.
func SetPodMetricsEnabled(v bool) {
	podMetricsEnabled = v
}

// PodGPUSample은 한 (Pod, GPU device) 조합의 메모리 관측치다.
// collector가 NVML RunningProcesses 결과를 (podUID, gpu) 키로 합산한 뒤 한 번에 RecordPodSnapshot으로 전달한다.
type PodGPUSample struct {
	ID           kube.PodIdentity
	Device       types.GPUDevice
	MemUsedBytes uint64
}

// lastPodSampleKeys는 직전 RecordPodSnapshot 호출에서 기록된 라벨 키 셋이다.
// diff-based cleanup에 쓰여, 이번 호출에 등장하지 않은 키는 DeleteLabelValues로 series에서 제거된다.
// 호출자는 단일 goroutine(collector pollOnce)이지만, 본 변수가 패키지 전역이고 RecordPodSnapshot이
// public 함수라 향후 호출 패턴 변경에 대비해 lastPodSampleKeysMu로 보호한다.
var (
	lastPodSampleKeys   = make(map[string]struct{})
	lastPodSampleKeysMu sync.Mutex
)

// seenCudaKeys 는 RecordCudaEvent 가 한 번이라도 기록한 (node, ns, pod, uid, gpu_uuid) 5-tuple
// 라벨 키 셋이다. cuda 카운터는 streaming event 기반이라 RecordPodSnapshot 류의 "이번 poll snapshot"
// 패턴이 적용되지 않는다. 그래서 RetainCudaSeries 호출 시점에 collector 가 NVML 풀링으로
// 산출한 active key 셋과 비교해, 더 이상 살아있지 않은 Pod 의 시리즈만 surgical Delete 한다.
//
// 키 타입은 고정 길이 [5]string 배열이라 strings.Join (RecordCudaEvent) / strings.Split (RetainCudaSeries)
// 류의 hot-path 힙 할당이 0 이고, 배열 자체가 hashable 이라 map 키로 직접 쓸 수 있다.
//
// hot path 인 RecordCudaEvent 는 동일 키가 반복 등장하므로 RWMutex 의 RLock fast path 로
// 매 이벤트의 write lock 비용을 회피한다. 새 키 등장 시에만 Lock 으로 승격한다.
var (
	seenCudaKeys   = make(map[CudaLabelKey]struct{})
	seenCudaKeysMu sync.RWMutex
)

// CudaLabelKey 는 cuda 카운터의 5 라벨 (node, src_namespace, src_pod, src_pod_uid, gpu_uuid) 고정
// 길이 배열이다. 배열 슬롯 순서는 RecordCudaEvent / CudaActiveKey / RetainCudaSeries 가 모두
// 공유하며, prometheus *Vec 의 WithLabelValues 호출 인자 순서와 정확히 일치해야 한다.
type CudaLabelKey [5]string

// podLabelSeparator는 라벨 키 직렬화용 구분자다. K8s 식별자(namespace/pod/uid 등)와 GPU UUID 어디에도
// 등장할 수 없는 NUL 바이트를 사용해 join/split 충돌을 회피한다.
const podLabelSeparator = "\x00"

var (
	deviceUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_utilization_percent",
			Help: "GPU compute utilization (0-100) sampled from NVML",
		},
		deviceLabels,
	)

	// migMode 는 #104 self-health gauge. 각 device 마다 3 mode (enabled/disabled/unsupported) 시리즈 를
	// 0/1 로 매 poll 발행 해 환경 변경 시점 까지 label 흐트러짐 없이 단일 query 로 fleet-wide MIG 분포 가시화.
	migMode = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_mig_mode",
			Help: "MIG mode of the GPU device (1 if matching the mode label, else 0). mode label one of enabled / disabled / unsupported. Use {mode=\"enabled\"} == 1 to count MIG-active devices fleet-wide",
		},
		migModeLabels,
	)

	// mpsActive 는 #104 self-health gauge. CUDA_MPS_PIPE_DIRECTORY env 와 /var/run/nvidia/mps/control
	// 소켓 존재 와 nvidia-cuda-mps-control process 3종 OR 로직 으로 detect 한 값 (1 / 0) 을 발행 한다.
	mpsActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_mps_active",
			Help: "1 if NVIDIA MPS daemon is active on the node, else 0. When 1, per-process SM utilization (gpuobs_pod_utilization_percent) is best-effort because MPS time-slices a single CUDA context across multiple processes which NVML cannot disaggregate",
		},
		mpsActiveLabels,
	)

	deviceMemoryUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_memory_used_bytes",
			Help: "GPU memory used in bytes sampled from NVML",
		},
		deviceLabels,
	)

	deviceMemoryTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_memory_total_bytes",
			Help: "GPU memory total capacity in bytes sampled from NVML",
		},
		deviceLabels,
	)

	deviceTemperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_temperature_celsius",
			Help: "GPU temperature in Celsius sampled from NVML",
		},
		deviceLabels,
	)

	devicePower = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_power_usage_watts",
			Help: "GPU power draw in watts sampled from NVML",
		},
		deviceLabels,
	)

	deviceMemoryCopyUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_memory_copy_utilization_percent",
			Help: "GPU memory copy engine utilization (0-100) sampled from NVML",
		},
		deviceLabels,
	)

	devicePcieRxBps = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_pcie_rx_bytes_per_second",
			Help: "Current PCIe receive throughput sampled by NVML over a 20ms window, normalized to bytes per second",
		},
		deviceLabels,
	)

	devicePcieTxBps = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_pcie_tx_bytes_per_second",
			Help: "Current PCIe transmit throughput sampled by NVML over a 20ms window, normalized to bytes per second",
		},
		deviceLabels,
	)

	deviceThrottleActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_throttle_active",
			Help: "Whether each NVML throttle reason is currently active (1) or not (0); reasons exposed as labels for sum/by aggregation",
		},
		deviceThrottleLabels,
	)

	deviceClockMhz = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_clock_mhz",
			Help: "Current GPU clock frequency in MHz per clock domain (sm/memory/graphics)",
		},
		deviceClockLabels,
	)

	deviceEccErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_device_ecc_errors_total",
			Help: "Cumulative ECC error count since gpuobs started, sourced from NVML VOLATILE counters; deltas applied between polls",
		},
		deviceEccLabels,
	)

	deviceEncoderUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_encoder_utilization_percent",
			Help: "GPU encoder (NVENC) utilization (0-100) sampled from NVML",
		},
		deviceLabels,
	)

	deviceDecoderUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_decoder_utilization_percent",
			Help: "GPU decoder (NVDEC) utilization (0-100) sampled from NVML",
		},
		deviceLabels,
	)

	devicePerformanceState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_performance_state",
			Help: "GPU performance state from NVML (0=highest, 15=idle, 32=unknown)",
		},
		deviceLabels,
	)

	deviceFanSpeed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_fan_speed_percent",
			Help: "GPU fan speed duty cycle (0-100) sampled from NVML; absent on passively-cooled cards",
		},
		deviceLabels,
	)

	deviceBAR1MemoryUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_bar1_memory_used_bytes",
			Help: "PCIe BAR1 memory area used in bytes sampled from NVML (host-mapped GPU memory)",
		},
		deviceLabels,
	)

	deviceBAR1MemoryTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_bar1_memory_total_bytes",
			Help: "PCIe BAR1 memory area capacity in bytes sampled from NVML",
		},
		deviceLabels,
	)

	devicePowerLimit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_power_limit_watts",
			Help: "Currently configured GPU power management limit in watts (NVML GetPowerManagementLimit)",
		},
		deviceLabels,
	)

	deviceThrottleViolationSeconds = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_device_throttle_violation_seconds_total",
			Help: "Cumulative throttle violation time per reason since gpuobs started, sourced from NVML GetViolationStatus; deltas applied between polls and converted from nanoseconds to seconds",
		},
		deviceViolationLabels,
	)

	deviceGpmUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_gpm_utilization_percent",
			Help: "GPU performance monitoring (GPM) utilization (0-100) per metric: graphics_util / sm_occupancy / tensor_active / dram_bandwidth. Datacenter GPU only; absent on consumer cards",
		},
		deviceGpmLabels,
	)

	deviceEnergyConsumption = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_device_energy_consumption_joules_total",
			Help: "Cumulative GPU energy consumption (joules) since the agent started, sourced from NVML GetTotalEnergyConsumption with baseline-then-delta tracking and millijoule-to-joule conversion",
		},
		deviceLabels,
	)

	devicePcieLinkGenerationCurrent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_pcie_link_generation_current",
			Help: "Current PCIe link generation negotiated by the GPU (1-5); idle GPUs may downgrade and recover under load",
		},
		deviceLabels,
	)

	devicePcieLinkWidthCurrent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_pcie_link_width_current",
			Help: "Current PCIe link width negotiated by the GPU (lanes 1-16); pairs with link_generation_current as runtime state",
		},
		deviceLabels,
	)

	devicePcieReplayErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_device_pcie_replay_errors_total",
			Help: "Cumulative PCIe link replay errors since the agent started, sourced from NVML GetPcieReplayCounter with baseline-then-delta tracking; sustained increase signals riser/cable/slot issues",
		},
		deviceLabels,
	)

	deviceTemperatureThreshold = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_temperature_threshold_celsius",
			Help: "GPU temperature threshold (celsius) per kind: slowdown / shutdown / mem_max / gpu_max. Static per device; pair with gpuobs_device_temperature_celsius for thermal headroom",
		},
		deviceTemperatureThresholdLabels,
	)

	devicePowerLimitEnforced = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_power_limit_enforced_watts",
			Help: "Currently enforced GPU power limit in watts (NVML GetEnforcedPowerLimit); usually equals power_limit_watts but may diverge under driver-level capping",
		},
		deviceLabels,
	)

	devicePersistenceMode = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_persistence_mode",
			Help: "NVML driver persistence mode (1=enabled, 0=disabled). Disabled mode incurs cold-start cost on first CUDA context creation",
		},
		deviceLabels,
	)

	deviceComputeMode = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_compute_mode",
			Help: "NVML compute mode enum (0=Default, 1=ExclusiveThread, 2=Prohibited, 3=ExclusiveProcess); diagnoses unintended exclusivity in multi-tenant environments",
		},
		deviceLabels,
	)

	deviceInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_info",
			Help: "Static GPU characteristics (compute capability / architecture / max PCIe spec / CUDA cores / memory bus width). Value is always 1; fleet-wide grouping is performed via labels",
		},
		deviceInfoLabels,
	)

	deviceFirmwareInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_device_firmware_info",
			Help: "GPU firmware versions (VBIOS / GSP firmware) for regression debugging. Value is always 1",
		},
		deviceFirmwareInfoLabels,
	)

	podMemoryUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_pod_memory_used_bytes",
			Help: "GPU memory used in bytes attributed to a single Pod via NVML running-process and cgroup-based PID resolution",
		},
		podLabels,
	)

	cudaKernelLaunchesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_cuda_kernel_launches_total",
			Help: "Cumulative count of CUDA kernel launches captured by uprobes on libcuda.so cuLaunchKernel/cuLaunchKernelEx/cuLaunchCooperativeKernel; attributed per Pod via cgroup-based PID resolution and per GPU via NVML running-process mapping",
		},
		cudaPodLabels,
	)

	cudaH2DBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_cuda_h2d_bytes_total",
			Help: "Cumulative bytes copied host→device via cuMemcpyHtoD_v2/cuMemcpyHtoDAsync_v2, captured by uprobes on libcuda.so",
		},
		cudaPodLabels,
	)

	cudaD2HBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_cuda_d2h_bytes_total",
			Help: "Cumulative bytes copied device→host via cuMemcpyDtoH_v2/cuMemcpyDtoHAsync_v2, captured by uprobes on libcuda.so",
		},
		cudaPodLabels,
	)

	cudaDtoDBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_cuda_dtod_bytes_total",
			Help: "Cumulative bytes copied device→device, aggregated across libcuda.so cuMemcpyDtoD_v2/cuMemcpyDtoDAsync_v2, cuMemcpy2D_v2/2DAsync_v2/3D_v2/3DAsync_v2 with device→device srcMemoryType/dstMemoryType, and libcudart cudaMemcpy/cudaMemcpyAsync with cudaMemcpyKind=DeviceToDevice",
		},
		cudaPodLabels,
	)

	cudaUnknownDirBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_cuda_unknown_dir_bytes_total",
			Help: "Cumulative bytes copied via memcpy paths whose direction the BPF layer cannot determine. Includes libcuda.so cuMemcpy (UVA, both ptrs are CUdeviceptr) and cuMemcpy2D/3D variants with ARRAY/UNIFIED/HOST→HOST memory-type combinations, plus libcudart cudaMemcpy/cudaMemcpyAsync invoked with cudaMemcpyKind=HostToHost or cudaMemcpyDefault. Sustained nonzero rate is a signal to investigate the workload with cuPointerGetAttribute outside this agent",
		},
		cudaPodLabels,
	)

	// #67 의 동기화 wait 시간 측정 메트릭 3종. histogram bucket 은 netobs stage latency 와 동일하게
	// 1us 부터 2배 곱 20 bucket 이며 alert 임계 100ms 가 17번째 경계 약 131ms 인근에서 정확한 p99
	// 산정이 가능하다. stream / event handle 은 cardinality 폭발 방지로 라벨에 포함되지 않으며 Pod
	// 단위 라벨 셋은 다른 cuda 메트릭과 동일하게 cudaPodLabels 를 그대로 쓴다.
	cudaStreamSynchronizeSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gpuobs_cuda_stream_synchronize_seconds",
			Help:    "Distribution of cuStreamSynchronize host wait time captured by libcuda.so uprobe / uretprobe pair. Indicates how long the calling host thread blocked waiting for all preceding work in a CUDA stream to complete. Attributed per Pod via cgroup-based PID resolution; the gpu_uuid label is resolved first from the BPF device_ord and visible-devices cache, then falls back to a PID-level devmap lookup, and finally to the literal 'unknown' if both fail",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		cudaPodLabels,
	)

	cudaEventSynchronizeSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gpuobs_cuda_event_synchronize_seconds",
			Help:    "Distribution of cuEventSynchronize host wait time captured by libcuda.so uprobe / uretprobe pair. Indicates how long the calling host thread blocked waiting for a CUDA event to be recorded. Attributed per Pod via cgroup-based PID resolution; the gpu_uuid label follows the same resolve chain as gpuobs_cuda_stream_synchronize_seconds (device_ord then PID-level devmap then 'unknown')",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		cudaPodLabels,
	)

	cudaStreamWaitEventTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_cuda_stream_wait_event_total",
			Help: "Cumulative count of cuStreamWaitEvent calls captured by libcuda.so uprobe. cuStreamWaitEvent is a non-blocking call that only enqueues a wait on the given stream, so it is exposed as a counter rather than a latency histogram. Use rate() to track inter-stream dependency frequency. gpu_uuid label follows the same resolve chain as gpuobs_cuda_stream_synchronize_seconds (device_ord then PID-level devmap then 'unknown')",
		},
		cudaPodLabels,
	)

	cudaSymbolAvailable = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_cuda_symbol_available",
			Help: "Whether each tracked libcuda.so / libcudart.so symbol was successfully resolved and uprobe-attached at agent startup (1=attached, 0=missing/failed/cudart-disabled); diagnoses driver/libcuda or libcudart ABI drift and cudart attach skipped via empty GPUOBS_CUDA_LIBCUDART_PATH",
		},
		cudaSymbolLabels,
	)

	cudaEventsLostTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gpuobs_cuda_events_lost_total",
			Help: "Cumulative count of CUDA uprobe ringbuf samples reported as lost by the kernel; sustained increase indicates the ring buffer is undersized for the current event rate",
		},
		[]string{"node"},
	)

	cudaPidMultiGPUCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_cuda_pid_multi_gpu_count",
			Help: "Number of host PIDs that were observed by NVML RunningProcesses on more than one GPU during the most recent device-map refresh. Per-GPU CUDA attribution is best-effort while this gauge is non-zero (see #33). DDP-style workloads with one process per GPU keep this gauge at 0; single-process multi-GPU usage (nn.DataParallel, model parallelism, custom multi-context CUDA programs) drives it above 0",
		},
		[]string{"node"},
	)

	// cudaLaunchBaselinePerSec 는 운영자가 의도한 워크로드의 기대 CUDA kernel launch rate (Hz) 를 정적
	// 으로 노출한다. correlation 진단의 pod:host_compute_stall_score:5m recording rule이 본 값을 분모로
	// 써서 실제 launch rate가 baseline 이하로 떨어진 비율을 0-1 score 로 정규화한다. CUDA_LAUNCH_
	// BASELINE_PER_SEC env / -cuda-launch-baseline CLI flag 로 노드별 override 한다. startup 1회 Set.
	cudaLaunchBaselinePerSec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_node_cuda_launch_baseline_per_sec",
			Help: "Expected CUDA kernel launch rate (Hz) per node, sourced from CUDA_LAUNCH_BASELINE_PER_SEC env (default 10). Consumed by correlation recording rules (pod:host_compute_stall_score:5m) as the host stall saturation denominator. Override per node via env to match workload characteristics (inference vs batch training).",
		},
		[]string{"node"},
	)
)

// Register는 gpuobs 지표를 주어진 Prometheus Registerer에 등록한다.
func Register(reg prometheus.Registerer) {
	agentInfo.Set(1)
	reg.MustRegister(
		agentInfo,
		deviceUtilization,
		migMode,
		mpsActive,
		deviceMemoryUsed,
		deviceMemoryTotal,
		deviceTemperature,
		devicePower,
		deviceMemoryCopyUtilization,
		devicePcieRxBps,
		devicePcieTxBps,
		deviceThrottleActive,
		deviceClockMhz,
		deviceEccErrors,
		deviceEncoderUtilization,
		deviceDecoderUtilization,
		devicePerformanceState,
		deviceFanSpeed,
		deviceBAR1MemoryUsed,
		deviceBAR1MemoryTotal,
		devicePowerLimit,
		deviceThrottleViolationSeconds,
		deviceGpmUtilization,
		deviceEnergyConsumption,
		devicePcieLinkGenerationCurrent,
		devicePcieLinkWidthCurrent,
		devicePcieReplayErrors,
		deviceTemperatureThreshold,
		devicePowerLimitEnforced,
		devicePersistenceMode,
		deviceComputeMode,
		deviceInfo,
		deviceFirmwareInfo,
		podMemoryUsed,
		cudaKernelLaunchesTotal,
		cudaH2DBytesTotal,
		cudaD2HBytesTotal,
		cudaDtoDBytesTotal,
		cudaUnknownDirBytesTotal,
		cudaStreamSynchronizeSeconds,
		cudaEventSynchronizeSeconds,
		cudaStreamWaitEventTotal,
		cudaSymbolAvailable,
		cudaEventsLostTotal,
		cudaPidMultiGPUCount,
		cudaLaunchBaselinePerSec,
		nvmlCallDuration,
		nvmlErrorsTotal,
		informerSyncLagSeconds,
	)
}

// SetCudaLaunchBaselinePerSec는 노드 CUDA launch baseline gauge를 startup 시점에 1회 설정한다. 본
// 메트릭은 scrape 시점마다 정적 값을 반환하므로 호출 횟수와 무관하게 cardinality 가 node 단위로
// 통제된다.
func SetCudaLaunchBaselinePerSec(node string, baseline float64) {
	cudaLaunchBaselinePerSec.WithLabelValues(node).Set(baseline)
}

// migModeValues 는 RecordMigMode 가 매 poll 발행할 mode 라벨 값 3종 enum 이다. types.MigMode.String()
// 의 반환값 집합과 정확히 일치 해야 활성 mode 시리즈가 1, 비활성 2 시리즈가 0 으로 정확히 정렬된다.
var migModeValues = [3]string{"enabled", "disabled", "unsupported"}

// RecordMigMode 는 #104 self-health 진입점이다. 한 device 의 현재 MigMode 에 대해 3 라벨 시리즈를
// 모두 발행하되 일치하는 mode 만 1, 나머지 2종은 0 으로 둬 라벨 전환 시 stale 시리즈가 남지 않게 한다.
func RecordMigMode(node string, dev types.GPUDevice) {
	idx := strconv.FormatUint(uint64(dev.Index), 10)
	current := dev.MigMode.String()
	for _, m := range migModeValues {
		v := 0.0
		if m == current {
			v = 1.0
		}
		migMode.WithLabelValues(node, dev.UUID, idx, dev.Model, m).Set(v)
	}
}

// RecordMpsActive 는 #104 self-health 진입점이다. detect 결과 (true=1 / false=0) 를 device 라벨 4종으로
// 발행한다. MPS 자체 는 노드 단위 신호 이지만 device 라벨 정합 유지 로 dashboard join 이 단순해진다.
func RecordMpsActive(node string, dev types.GPUDevice, active bool) {
	idx := strconv.FormatUint(uint64(dev.Index), 10)
	v := 0.0
	if active {
		v = 1.0
	}
	mpsActive.WithLabelValues(node, dev.UUID, idx, dev.Model).Set(v)
}

// Record는 한 device의 현재 스냅샷을 모든 device gauge에 기록한다.
// 인자 순서는 deviceLabels({node, gpu_uuid, gpu_index, gpu_model})와 정확히 일치해야 하며,
// 매 호출 시 `prometheus.Labels` 맵 할당을 피하기 위해 `WithLabelValues`를 사용한다.
// 라벨 카디널리티는 노드당 device 수(통상 ≤8)로 제한되어 별도 escape hatch는 두지 않는다.
//
// 신규 device 메트릭(PCIe / throttle / clock / ECC / encoder/decoder / pstate)은 각 *Supported
// 플래그가 true일 때만 발행한다. 미지원 GPU에서는 해당 시리즈가 처음부터 만들어지지 않아
// Prometheus 카디널리티가 늘어나지 않는다.
func Record(node string, snap types.GPUSnapshot) {
	idx := strconv.FormatUint(uint64(snap.Device.Index), 10)
	uuid := snap.Device.UUID
	model := snap.Device.Model

	deviceUtilization.WithLabelValues(node, uuid, idx, model).Set(float64(snap.UtilizationPct))
	deviceMemoryUsed.WithLabelValues(node, uuid, idx, model).Set(float64(snap.MemoryUsedBytes))
	deviceMemoryTotal.WithLabelValues(node, uuid, idx, model).Set(float64(snap.MemoryTotalBytes))
	deviceTemperature.WithLabelValues(node, uuid, idx, model).Set(float64(snap.TemperatureC))
	devicePower.WithLabelValues(node, uuid, idx, model).Set(snap.PowerUsageWatts)
	// MemoryCopyUtilPct는 GetUtilizationRates 단일 호출에서 UtilizationPct(.Gpu)와 함께 반환되는 값(.Memory)이라
	// 별도 *Supported 게이트 없이 base 메트릭과 같은 NVML SUCCESS gate 아래 발행한다. 호출 자체가 실패하면
	// Snapshot이 상위에서 에러로 빠져나가므로 여기까지 도달했다는 것은 두 값 모두 유효하다는 뜻이다.
	deviceMemoryCopyUtilization.WithLabelValues(node, uuid, idx, model).Set(float64(snap.MemoryCopyUtilPct))

	if snap.PcieSupported {
		devicePcieRxBps.WithLabelValues(node, uuid, idx, model).Set(float64(snap.PcieRxBps))
		devicePcieTxBps.WithLabelValues(node, uuid, idx, model).Set(float64(snap.PcieTxBps))
	}

	if snap.ThrottleReasonsSupported {
		// known reason 9종을 매 poll마다 0/1로 모두 발행해 stale 라벨을 자연 회피한다.
		for _, r := range throttleReasonBits {
			active := 0.0
			if snap.ThrottleReasons&r.bit != 0 {
				active = 1.0
			}
			deviceThrottleActive.WithLabelValues(node, uuid, idx, model, r.name).Set(active)
		}
	}

	if snap.ClockSMSupported {
		deviceClockMhz.WithLabelValues(node, uuid, idx, model, "sm").Set(float64(snap.ClockSMMhz))
	}
	if snap.ClockMemorySupported {
		deviceClockMhz.WithLabelValues(node, uuid, idx, model, "memory").Set(float64(snap.ClockMemoryMhz))
	}
	if snap.ClockGraphicsSupported {
		deviceClockMhz.WithLabelValues(node, uuid, idx, model, "graphics").Set(float64(snap.ClockGraphicsMhz))
	}

	if snap.EccSupported {
		recordEccDelta(node, uuid, idx, model, "corrected", snap.EccCorrectedTotal)
		recordEccDelta(node, uuid, idx, model, "uncorrected", snap.EccUncorrectedTotal)
	}

	if snap.EncoderSupported {
		deviceEncoderUtilization.WithLabelValues(node, uuid, idx, model).Set(float64(snap.EncoderUtilPct))
	}
	if snap.DecoderSupported {
		deviceDecoderUtilization.WithLabelValues(node, uuid, idx, model).Set(float64(snap.DecoderUtilPct))
	}

	if snap.PerformanceStateSupported {
		devicePerformanceState.WithLabelValues(node, uuid, idx, model).Set(float64(snap.PerformanceState))
	}

	if snap.FanSpeedSupported {
		deviceFanSpeed.WithLabelValues(node, uuid, idx, model).Set(float64(snap.FanSpeedPct))
	}

	if snap.BAR1Supported {
		deviceBAR1MemoryUsed.WithLabelValues(node, uuid, idx, model).Set(float64(snap.BAR1MemoryUsedBytes))
		deviceBAR1MemoryTotal.WithLabelValues(node, uuid, idx, model).Set(float64(snap.BAR1MemoryTotalBytes))
	}

	if snap.PowerLimitSupported {
		devicePowerLimit.WithLabelValues(node, uuid, idx, model).Set(snap.PowerLimitWatts)
	}

	if snap.ViolationSupported {
		for reason, ns := range snap.ViolationTimesNs {
			recordViolationDelta(node, uuid, idx, model, reason, ns)
		}
	}

	// GPM은 두 sample이 모인 두 번째 poll부터 metric 산출이 가능하다. GpmFirstSampleReady=false인
	// 첫 poll은 발행을 건너뛰고 baseline sample만 nvml 계층이 보관한다.
	if snap.GpmSupported && snap.GpmFirstSampleReady {
		deviceGpmUtilization.WithLabelValues(node, uuid, idx, model, "graphics_util").Set(snap.GpmGraphicsUtilPct)
		deviceGpmUtilization.WithLabelValues(node, uuid, idx, model, "sm_occupancy").Set(snap.GpmSMOccupancyPct)
		deviceGpmUtilization.WithLabelValues(node, uuid, idx, model, "tensor_active").Set(snap.GpmTensorActivePct)
		deviceGpmUtilization.WithLabelValues(node, uuid, idx, model, "dram_bandwidth").Set(snap.GpmDramBandwidthPct)
	}

	if snap.EnergySupported {
		recordEnergyDelta(node, uuid, idx, model, snap.EnergyConsumptionMilliJoules)
	}

	if snap.PcieLinkSupported {
		devicePcieLinkGenerationCurrent.WithLabelValues(node, uuid, idx, model).Set(float64(snap.PcieLinkGenerationCurrent))
		devicePcieLinkWidthCurrent.WithLabelValues(node, uuid, idx, model).Set(float64(snap.PcieLinkWidthCurrent))
	}

	if snap.PcieReplaySupported {
		recordPcieReplayDelta(node, uuid, idx, model, snap.PcieReplayErrors)
	}

	if snap.TemperatureThresholdSupported {
		for thresholdName, celsius := range snap.TemperatureThresholdsCelsius {
			deviceTemperatureThreshold.WithLabelValues(node, uuid, idx, model, thresholdName).Set(float64(celsius))
		}
	}

	if snap.PowerLimitEnforcedSupported {
		devicePowerLimitEnforced.WithLabelValues(node, uuid, idx, model).Set(snap.PowerLimitEnforcedWatts)
	}
	if snap.PersistenceModeSupported {
		devicePersistenceMode.WithLabelValues(node, uuid, idx, model).Set(float64(snap.PersistenceModeEnabled))
	}
	if snap.ComputeModeSupported {
		deviceComputeMode.WithLabelValues(node, uuid, idx, model).Set(float64(snap.ComputeMode))
	}

	// 정적 device 특성과 펌웨어 정보는 매 poll 같은 라벨 값으로 idempotent Set한다.
	// 라벨 값이 device 수명 동안 불변이므로 동일 시리즈가 유지되고, 일부 NVML 호출이 init에서 실패해 zero value로 남은 필드는
	// 정수 필드는 "0", 문자열 필드는 fallbackString으로 "unknown" 으로 치환되어 카디널리티 폭증 없이 device당 1 시리즈만 노출된다.
	deviceInfo.WithLabelValues(
		node, uuid, idx, model,
		formatComputeCapability(snap.Device.CudaComputeMajor, snap.Device.CudaComputeMinor),
		fallbackString(snap.Device.Architecture, "unknown"),
		strconv.Itoa(snap.Device.MaxPcieLinkGeneration),
		strconv.Itoa(snap.Device.MaxPcieLinkWidth),
		strconv.Itoa(snap.Device.NumGpuCores),
		strconv.FormatUint(uint64(snap.Device.MemoryBusWidthBits), 10),
	).Set(1)
	deviceFirmwareInfo.WithLabelValues(
		node, uuid, idx, model,
		fallbackString(snap.Device.VbiosVersion, "unknown"),
		fallbackString(snap.Device.GspFirmwareVersion, "unknown"),
	).Set(1)
}

// formatComputeCapability는 (major, minor) 를 "8.6" 형식으로 합친다. 두 값 모두 0이면 "unknown" 으로 둔다.
func formatComputeCapability(major, minor int) string {
	if major == 0 && minor == 0 {
		return "unknown"
	}
	return strconv.Itoa(major) + "." + strconv.Itoa(minor)
}

// fallbackString은 빈 문자열을 fallback 으로 치환한다. NVML 호출 실패로 미수집된 정적 필드가 라벨에 빈 값으로 들어가는 것을 방지한다.
func fallbackString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// recordEccDelta는 NVML이 보고한 누적 절대값에서 직전 poll의 값을 빼 양수 delta만 Counter에 더한다.
// 첫 호출(prev 미존재)은 baseline만 저장하고 Add를 건너뛴다 — 에이전트 기동 이전에 이미 누적되어
// 있던 NVML VOLATILE 값이 한 번에 counter로 반영되지 않게 해, "에이전트 기동 이후 신규 ECC"라는
// counter 의미(README 명시)를 정확히 보존한다.
// current < prev (드라이버 리셋 / GPU 리셋 등) 시에는 데이터 연속성 단절로 간주해 가산을 건너뛰고 새 baseline 으로만 갱신한다 — 거짓 spike 회피.
// lastEccAbsoluteMu가 패키지 전역 map 접근을 보호한다.
func recordEccDelta(node, uuid, idx, model, errorType string, current uint64) {
	key := uuid + "/" + errorType

	lastEccAbsoluteMu.Lock()
	defer lastEccAbsoluteMu.Unlock()

	prev, ok := lastEccAbsolute[key]
	if !ok {
		lastEccAbsolute[key] = current
		return
	}
	if current < prev {
		// 드라이버 리셋 등으로 NVML 누적값이 감소한 경우 데이터 연속성이 끊긴 것으로 간주한다.
		// 해당 poll의 값을 fresh delta로 더하면 reset 직전까지의 누적분과 reset 직후 부분 누적이 합쳐져 dashboard에
		// 거짓 spike를 만들 수 있으므로, 이번 poll의 가산은 건너뛰고 새 baseline으로만 둔 뒤 다음 poll부터 정상 delta를 적용한다.
		lastEccAbsolute[key] = current
		return
	}
	if delta := current - prev; delta > 0 {
		deviceEccErrors.WithLabelValues(node, uuid, idx, model, errorType).Add(float64(delta))
	}
	lastEccAbsolute[key] = current
}

// recordViolationDelta는 NVML이 보고한 누적 ns 절대값에서 직전 poll 값을 빼 양수 delta만 seconds로 환산해 Counter에 더한다.
// recordEccDelta와 동일한 baseline-then-delta 패턴이며, "에이전트 기동 이후 신규 throttle 시간"이라는 counter 의미를 보존한다.
// current < prev (드라이버 리셋 / GPU 리셋 등) 시에는 데이터 연속성이 끊긴 것으로 간주하고 가산을 건너뛴 뒤
// 새 baseline 으로만 갱신해 dashboard 거짓 spike를 회피한다 (다음 poll 부터 정상 delta 가산).
// lastViolationAbsoluteMu가 패키지 전역 map 접근을 보호한다.
func recordViolationDelta(node, uuid, idx, model, reason string, currentNs uint64) {
	key := uuid + "/" + reason

	lastViolationAbsoluteMu.Lock()
	defer lastViolationAbsoluteMu.Unlock()

	prev, ok := lastViolationAbsolute[key]
	if !ok {
		lastViolationAbsolute[key] = currentNs
		return
	}
	if currentNs < prev {
		lastViolationAbsolute[key] = currentNs
		return
	}
	if deltaNs := currentNs - prev; deltaNs > 0 {
		// nanoseconds → seconds 환산. Counter 이름이 `_seconds_total`이므로 단위 일관성을 보장한다.
		deviceThrottleViolationSeconds.WithLabelValues(node, uuid, idx, model, reason).Add(float64(deltaNs) / 1e9)
	}
	lastViolationAbsolute[key] = currentNs
}

// recordEnergyDelta는 NVML이 보고한 누적 mJ 절대값에서 직전 poll 값을 빼 양수 delta만 J로 환산해 Counter에 더한다.
// recordEccDelta / recordViolationDelta 와 동일한 baseline-then-delta 패턴 + reset skip 정책 적용.
// 단위 환산만 mJ → J(÷1000) 로 다르며, current < prev (드라이버 reload 등) 시에는 가산을 건너뛰고 새 baseline 만 갱신한다.
// lastEnergyAbsoluteMu가 패키지 전역 map 접근을 보호한다.
func recordEnergyDelta(node, uuid, idx, model string, currentMilliJoules uint64) {
	key := uuid

	lastEnergyAbsoluteMu.Lock()
	defer lastEnergyAbsoluteMu.Unlock()

	prev, ok := lastEnergyAbsolute[key]
	if !ok {
		lastEnergyAbsolute[key] = currentMilliJoules
		return
	}
	if currentMilliJoules < prev {
		lastEnergyAbsolute[key] = currentMilliJoules
		return
	}
	if deltaMilliJoules := currentMilliJoules - prev; deltaMilliJoules > 0 {
		// millijoules → joules 환산. Counter 이름이 `_joules_total` 이므로 단위 일관성을 보장한다.
		deviceEnergyConsumption.WithLabelValues(node, uuid, idx, model).Add(float64(deltaMilliJoules) / 1000.0)
	}
	lastEnergyAbsolute[key] = currentMilliJoules
}

// recordPcieReplayDelta는 NVML 누적 PCIe replay 카운터에서 직전 poll 값을 빼 양수 delta만 Counter에 더한다.
// uint32 wrap-around 또는 driver reload 로 인한 reset 시에는 가산을 건너뛰고 새 baseline 만 갱신한다 (다른 counter 와 동일 정책).
func recordPcieReplayDelta(node, uuid, idx, model string, current uint32) {
	key := uuid

	lastPcieReplayAbsoluteMu.Lock()
	defer lastPcieReplayAbsoluteMu.Unlock()

	prev, ok := lastPcieReplayAbsolute[key]
	if !ok {
		lastPcieReplayAbsolute[key] = current
		return
	}
	if current < prev {
		lastPcieReplayAbsolute[key] = current
		return
	}
	if delta := current - prev; delta > 0 {
		devicePcieReplayErrors.WithLabelValues(node, uuid, idx, model).Add(float64(delta))
	}
	lastPcieReplayAbsolute[key] = current
}

// RecordPodSnapshot은 이번 poll에 관측된 (Pod, GPU) 메모리 사용량 스냅샷을 일괄 기록한다.
// 호출자(collector)는 NVML RunningProcesses 결과를 (podUID, gpu) 단위로 합산한 뒤 본 함수 한 번만 호출한다.
// 이로써 동일 Pod의 다중 GPU 프로세스가 라벨 충돌로 덮어써지는 문제를 막는다.
//
// 직전 호출에서는 있었지만 이번에는 없는 라벨 시리즈는 DeleteLabelValues로 surgical하게 제거되어
// 종료된 Pod의 stale gauge가 영구히 남는 것도 방지한다. Reset()을 쓰지 않아 scrape 중간에 빈 시리즈가
// 보이는 race window를 회피한다.
//
// podMetricsEnabled가 false이면 신규 기록은 건너뛰지만 직전 라벨 cleanup은 그대로 수행해
// 토글 off 직후 잔존 series가 즉시 정리되도록 한다.
//
// 호출자는 단일 goroutine(pollOnce)을 가정하지만 본 함수가 public이고 lastPodSampleKeys가 패키지
// 전역이라 향후 호출 패턴 변경에 대비해 lastPodSampleKeysMu로 보호한다.
func RecordPodSnapshot(node string, samples []PodGPUSample) {
	currentKeys := make(map[string]struct{}, len(samples))

	if podMetricsEnabled {
		for _, s := range samples {
			if !s.ID.IsPod() {
				continue
			}
			idx := strconv.FormatUint(uint64(s.Device.Index), 10)
			labels := []string{
				node,
				s.ID.NamespaceLabel(),
				podName(s.ID),
				podUID(s.ID),
				s.Device.UUID,
				idx,
			}
			podMemoryUsed.WithLabelValues(labels...).Set(float64(s.MemUsedBytes))
			currentKeys[strings.Join(labels, podLabelSeparator)] = struct{}{}
		}
	}

	// 직전 poll에는 있었지만 이번에는 없는 라벨 series 제거 (Pod 종료 / 프로세스 종료 / toggle off 모두 흡수).
	lastPodSampleKeysMu.Lock()
	defer lastPodSampleKeysMu.Unlock()
	for key := range lastPodSampleKeys {
		if _, ok := currentKeys[key]; !ok {
			podMemoryUsed.DeleteLabelValues(strings.Split(key, podLabelSeparator)...)
		}
	}
	lastPodSampleKeys = currentKeys
}

// podName과 podUID는 빈 필드일 때 "unknown"으로 폴백해 라벨 카디널리티가 빈 문자열로 늘어나는 것을 막는다.
// netobs metrics와 동일한 폴백 정책을 사용한다. 외부 패키지가 동일 라벨 키를 만들어야 할 때 (예: cuda 패키지의
// RetainCudaSeries 호출용 active key 생성) 같은 폴백 적용을 보장하기 위해 PodNameOrUnknown / PodUIDOrUnknown
// 으로도 노출한다 — 두 곳에서 다른 폴백을 쓰면 cleanup 키 매칭이 깨져 stale 시리즈가 영원히 남는다.
func podName(id kube.PodIdentity) string { return PodNameOrUnknown(id) }
func podUID(id kube.PodIdentity) string  { return PodUIDOrUnknown(id) }

// PodNameOrUnknown 은 id.PodName 이 비어 있을 때 "unknown" 으로 폴백한다.
// metrics 측 podName 과 외부 호출자 (cuda 패키지) 의 active key 생성 로직이 동일 폴백을 공유하도록 노출된다.
func PodNameOrUnknown(id kube.PodIdentity) string {
	if id.PodName != "" {
		return id.PodName
	}
	return "unknown"
}

// PodUIDOrUnknown 은 id.PodUID 이 비어 있을 때 "unknown" 으로 폴백한다. PodNameOrUnknown 과 같은 이유로 노출된다.
func PodUIDOrUnknown(id kube.PodIdentity) string {
	if id.PodUID != "" {
		return id.PodUID
	}
	return "unknown"
}

// CudaEventSample 은 CUDA uprobe 가 캡처한 이벤트 한 건을 PID→Pod 귀속과 PID→GPU 매핑까지
// 끝낸 결과물이다. cuda 패키지의 reader 가 BPF 이벤트 → kube.PodIdentity → GPU UUID 해상도를
// 모두 마친 뒤 본 구조체로 채워 metrics 계층에 넘긴다.
type CudaEventSample struct {
	ID      kube.PodIdentity
	GPUUUID string
	Kind    types.CudaEventKind
	Bytes   uint64
	// LatencyNs 는 #67 의 동기화 wait 시간이다. StreamSync / EventSync kind 만 entry-exit 페어가
	// 산정한 ns 값을 담고 그 외 kind 는 0 으로 둔다.
	LatencyNs uint64
}

// RecordCudaEvent 는 단일 cuda 이벤트를 적절한 카운터에 누적한다.
//   - kernel launch         → kernel_launches_total +1
//   - h2d / d2h / dtod      → 각 *_bytes_total += event.Bytes
//   - unknown_dir           → unknown_dir_bytes_total += event.Bytes
//
// IsPod() 가 false 인 식별자는 비-Pod 호스트 프로세스로 간주해 발행을 건너뛴다 (RecordPodSnapshot 과 동일 정책).
// GPUUUID 가 비어 있으면 "unknown" 으로 폴백해 빈 라벨 카디널리티를 막는다.
// 사용한 라벨 키는 seenCudaKeys 에 기록되어 추후 RetainCudaSeries 가 stale series 를 제거할 때 참조한다.
func RecordCudaEvent(node string, s CudaEventSample) {
	if !s.ID.IsPod() {
		return
	}
	// stack-allocated 5-tuple. WithLabelValues 에 labels[:] 로 넘겨도 backing array 가 stack 에
	// 머물러 strings.Join 류의 힙 할당을 발생시키지 않는다.
	labels := CudaLabelKey{
		node,
		s.ID.NamespaceLabel(),
		podName(s.ID),
		podUID(s.ID),
		fallbackString(s.GPUUUID, "unknown"),
	}

	switch s.Kind {
	case types.CudaEventKernelLaunch:
		cudaKernelLaunchesTotal.WithLabelValues(labels[:]...).Inc()
	case types.CudaEventH2D:
		cudaH2DBytesTotal.WithLabelValues(labels[:]...).Add(float64(s.Bytes))
	case types.CudaEventD2H:
		cudaD2HBytesTotal.WithLabelValues(labels[:]...).Add(float64(s.Bytes))
	case types.CudaEventDtoD:
		cudaDtoDBytesTotal.WithLabelValues(labels[:]...).Add(float64(s.Bytes))
	case types.CudaEventUnknownDir:
		cudaUnknownDirBytesTotal.WithLabelValues(labels[:]...).Add(float64(s.Bytes))
	case types.CudaEventStreamSync:
		cudaStreamSynchronizeSeconds.WithLabelValues(labels[:]...).Observe(float64(s.LatencyNs) / 1e9)
	case types.CudaEventEventSync:
		cudaEventSynchronizeSeconds.WithLabelValues(labels[:]...).Observe(float64(s.LatencyNs) / 1e9)
	case types.CudaEventStreamWaitEvent:
		cudaStreamWaitEventTotal.WithLabelValues(labels[:]...).Inc()
	default:
		// 정의되지 않은 kind 는 BPF / userspace enum 이 어긋난 신호라 발행을 건너뛴다.
		return
	}

	// hot path: 이미 본 키면 RLock 만으로 끝나고, 신규 키일 때만 write lock 으로 승격한다.
	seenCudaKeysMu.RLock()
	_, seen := seenCudaKeys[labels]
	seenCudaKeysMu.RUnlock()
	if seen {
		return
	}
	seenCudaKeysMu.Lock()
	seenCudaKeys[labels] = struct{}{}
	seenCudaKeysMu.Unlock()
}

// AddCudaEventsLost 는 cuda 패키지의 ringbuf reader 가 record.LostSamples 로 알려준 누락 이벤트
// 수를 누적시킨다. 호출 측은 이미 0 가드를 거치므로 본 함수는 양수만 받는다고 가정한다.
func AddCudaEventsLost(node string, lost uint64) {
	cudaEventsLostTotal.WithLabelValues(node).Add(float64(lost))
}

// CudaActiveKey 는 RetainCudaSeries 호출자가 살아있다고 보고하는 라벨 셋의 5-tuple 키를 만든다.
// RecordCudaEvent 가 사용하는 키 슬롯 순서와 정확히 동일해야 cleanup 비교가 일관된다.
func CudaActiveKey(node, namespace, podName, podUID, gpuUUID string) CudaLabelKey {
	return CudaLabelKey{
		node,
		namespace,
		podName,
		podUID,
		fallbackString(gpuUUID, "unknown"),
	}
}

// RetainCudaSeries 는 collector 가 이번 NVML 풀링에서 활성으로 식별한 (Pod, GPU) 라벨 키 셋
// (activeKeys) 만 남기고, seenCudaKeys 에는 있지만 activeKeys 에는 없는 시리즈 — 즉 종료된 Pod /
// 종료된 프로세스 — 의 카운터 5종(kernel / h2d / d2h / dtod / unknown_dir) 시리즈를 surgical Delete
// 한다. Reset() 을 쓰지 않아 scrape 도중 빈 시리즈가 잠깐 노출되는 race 를 회피한다.
//
// 단점: NVML 풀링 주기보다 짧은 수명의 GPU 프로세스는 한 번도 activeKeys 에 들어가지 못해 시리즈가
// 기록 직후 다음 cleanup 에서 제거될 수 있다. 컨슈머 카드 vectorAdd 류 단발 워크로드에 한해 발생하며,
// 이 한계는 README 에 명시한다.
func RetainCudaSeries(activeKeys map[CudaLabelKey]struct{}) {
	seenCudaKeysMu.Lock()
	defer seenCudaKeysMu.Unlock()
	for key := range seenCudaKeys {
		if _, ok := activeKeys[key]; ok {
			continue
		}
		labels := key[:]
		cudaKernelLaunchesTotal.DeleteLabelValues(labels...)
		cudaH2DBytesTotal.DeleteLabelValues(labels...)
		cudaD2HBytesTotal.DeleteLabelValues(labels...)
		cudaDtoDBytesTotal.DeleteLabelValues(labels...)
		cudaUnknownDirBytesTotal.DeleteLabelValues(labels...)
		cudaStreamSynchronizeSeconds.DeleteLabelValues(labels...)
		cudaEventSynchronizeSeconds.DeleteLabelValues(labels...)
		cudaStreamWaitEventTotal.DeleteLabelValues(labels...)
		delete(seenCudaKeys, key)
	}
}

// SetCudaPidMultiGPUCount 는 NVML refresh 사이클이 활성 PID 셋을 GPU 별 RunningProcesses 결과로 구성하면서
// 복수 GPU 에 동시 등장한 PID 수를 매 사이클마다 발행한다. 본 gauge 가 0 이 아니라는 것은 한 PID 가 동시에
// 여러 GPU 를 사용해 현재의 단일값 pidToUUID 매핑이 last-GPU-wins 로 동작 중이라는 신호다 (#33).
// DDP 류 GPU 당 1 프로세스 패턴은 NVML 이 PID 를 한 GPU 에만 등록하므로 0 으로 유지된다.
func SetCudaPidMultiGPUCount(node string, count int) {
	cudaPidMultiGPUCount.WithLabelValues(node).Set(float64(count))
}

// SetCudaSymbolAvailability 는 libcuda 심볼 한 개의 uprobe attach 결과를 0/1 gauge 로 발행한다.
// 에이전트 기동 시 uprobe attach 실행 직후 1회 호출되며, 시간이 지나도 변하지 않는 정적 시그널이다.
// 라벨이 (node, symbol) 두 개 뿐이라 동일 라벨 셋의 idempotent Set 이며 series 가 늘어나지 않는다.
func SetCudaSymbolAvailability(node, symbol string, available bool) {
	v := 0.0
	if available {
		v = 1.0
	}
	cudaSymbolAvailable.WithLabelValues(node, symbol).Set(v)
}
