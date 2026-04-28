// Package metrics는 gpuobs 에이전트가 Prometheus로 발행하는 지표를 정의한다.
// gpuobs 전용 프리픽스 `gpuobs_`를 써서 netobs 지표(`netobs_*`)와 네임스페이스를 분리한다.
package metrics

import (
	"strconv"
	"strings"

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

// podLabels는 per-pod gauge의 공통 라벨 세트다. 앞 4개(node/src_namespace/src_pod/src_pod_uid)는
// netobs `netobs_pod_stage_events_labeled_total`과 정확히 일치해 PromQL 조인 키로 쓰일 수 있다.
// gpu_uuid/gpu_index는 GPU 차원을 추가해 한 Pod이 복수 GPU를 사용하는 경우 분리 측정한다.
var podLabels = []string{"node", "src_namespace", "src_pod", "src_pod_uid", "gpu_uuid", "gpu_index"}

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
// 이 변수는 단일 호출자(collector pollOnce)만 접근한다는 startup-only 계약 아래 별도 mutex 없이 운영된다.
var lastPodSampleKeys = make(map[string]struct{})

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

	podMemoryUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gpuobs_pod_memory_used_bytes",
			Help: "GPU memory used in bytes attributed to a single Pod via NVML running-process and cgroup-based PID resolution",
		},
		podLabels,
	)
)

// Register는 gpuobs 지표를 주어진 Prometheus Registerer에 등록한다.
func Register(reg prometheus.Registerer) {
	agentInfo.Set(1)
	reg.MustRegister(
		agentInfo,
		deviceUtilization,
		deviceMemoryUsed,
		deviceMemoryTotal,
		deviceTemperature,
		devicePower,
		podMemoryUsed,
	)
}

// Record는 한 device의 현재 스냅샷을 모든 device gauge에 기록한다.
// 인자 순서는 deviceLabels({node, gpu_uuid, gpu_index, gpu_model})와 정확히 일치해야 하며,
// 매 호출 시 `prometheus.Labels` 맵 할당을 피하기 위해 `WithLabelValues`를 사용한다.
// 라벨 카디널리티는 노드당 device 수(통상 ≤8)로 제한되어 별도 escape hatch는 두지 않는다.
func Record(node string, snap types.GPUSnapshot) {
	idx := strconv.FormatUint(uint64(snap.Device.Index), 10)
	uuid := snap.Device.UUID
	model := snap.Device.Model

	deviceUtilization.WithLabelValues(node, uuid, idx, model).Set(float64(snap.UtilizationPct))
	deviceMemoryUsed.WithLabelValues(node, uuid, idx, model).Set(float64(snap.MemoryUsedBytes))
	deviceMemoryTotal.WithLabelValues(node, uuid, idx, model).Set(float64(snap.MemoryTotalBytes))
	deviceTemperature.WithLabelValues(node, uuid, idx, model).Set(float64(snap.TemperatureC))
	devicePower.WithLabelValues(node, uuid, idx, model).Set(snap.PowerUsageWatts)
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
// startup-only 계약: 호출자는 단일 goroutine(pollOnce)에서만 본 함수를 부른다고 가정하며 별도 lock을 잡지 않는다.
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
	for key := range lastPodSampleKeys {
		if _, ok := currentKeys[key]; !ok {
			podMemoryUsed.DeleteLabelValues(strings.Split(key, podLabelSeparator)...)
		}
	}
	lastPodSampleKeys = currentKeys
}

// podName과 podUID는 빈 필드일 때 "unknown"으로 폴백해 라벨 카디널리티가 빈 문자열로 늘어나는 것을 막는다.
// netobs metrics와 동일한 폴백 정책을 사용한다.
func podName(id kube.PodIdentity) string {
	if id.PodName != "" {
		return id.PodName
	}
	return "unknown"
}

func podUID(id kube.PodIdentity) string {
	if id.PodUID != "" {
		return id.PodUID
	}
	return "unknown"
}
