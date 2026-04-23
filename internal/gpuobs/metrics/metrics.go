// Package metrics는 gpuobs 에이전트가 Prometheus로 발행하는 지표를 정의한다.
// gpuobs 전용 프리픽스 `gpuobs_`를 써서 netobs 지표(`netobs_*`)와 네임스페이스를 분리한다.
package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/gpuobs/types"
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
