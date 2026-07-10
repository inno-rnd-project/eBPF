package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// GpuStatusResponse 는 GET /api/v1/gpu-status 의 typed 응답이다. gpu-idle 이 "왜 노는가" 를 다루는
// 것과 별개로 "지금 GPU 가 어떤 상태인가" (사용률과 메모리, 전력, 온도, throttle, 점유 pod) 의 현황
// 스냅샷을 device 단위로 합성한다. gpuobs_device_* / gpuobs_pod_* instant query 만 사용하며 recording
// rule 에 의존하지 않는다.
type GpuStatusResponse struct {
	GeneratedAt string      `json:"generated_at"`
	Devices     []GpuDevice `json:"devices"`
	Summary     string      `json:"summary"`
}

// GpuDevice 는 한 GPU device 의 현황이다. 주 소스인 사용률 외 신호는 수집 공백 시 필드가 생략될 수
// 있도록 pointer 로 둔다 (NaN 은 JSON 직렬화가 불가해 omitempty pointer 규약을 따른다).
type GpuDevice struct {
	Node               string   `json:"node"`
	GpuUUID            string   `json:"gpu_uuid"`
	GpuIndex           string   `json:"gpu_index"`
	Model              string   `json:"model,omitempty"`
	UtilizationPercent float64  `json:"utilization_percent"`
	MemoryUsedBytes    *float64 `json:"memory_used_bytes,omitempty"`
	MemoryTotalBytes   *float64 `json:"memory_total_bytes,omitempty"`
	MemoryUsedRatio    *float64 `json:"memory_used_ratio,omitempty"`
	PowerUsageWatts    *float64 `json:"power_usage_watts,omitempty"`
	PowerLimitWatts    *float64 `json:"power_limit_watts,omitempty"`
	TemperatureCelsius *float64 `json:"temperature_celsius,omitempty"`
	// 아래는 #267 의 device 상세 확장 필드다. gpuobs 가 수집하나 기존 gpu-status 가 노출하지 않던
	// 신호로, heatmap 온도 위험도 색칠 (temperature_thresholds_celsius) 과 GPU Detail 페이지
	// (클럭, 팬, PCIe, performance state 등) 의 데이터를 채운다. 전부 수집 공백 시 생략되도록
	// pointer 또는 omitempty map 이다.
	//
	// SMActivePercent 는 gpm_utilization_percent 의 sm_occupancy 다. consumer GPU (RTX 등) 는 GPM
	// 미지원이라 빈 값이고 데이터센터 GPU (A100+) 에서만 채워진다.
	SMActivePercent           *float64 `json:"sm_active_percent,omitempty"`
	EncoderUtilizationPercent *float64 `json:"encoder_utilization_percent,omitempty"`
	DecoderUtilizationPercent *float64 `json:"decoder_utilization_percent,omitempty"`
	Bar1MemoryUsedBytes       *float64 `json:"bar1_memory_used_bytes,omitempty"`
	Bar1MemoryTotalBytes      *float64 `json:"bar1_memory_total_bytes,omitempty"`
	PowerLimitEnforcedWatts   *float64 `json:"power_limit_enforced_watts,omitempty"`
	EnergyConsumptionJoules   *float64 `json:"energy_consumption_joules,omitempty"`
	FanSpeedPercent           *float64 `json:"fan_speed_percent,omitempty"`
	PerformanceState          *float64 `json:"performance_state,omitempty"`
	ComputeMode               *float64 `json:"compute_mode,omitempty"`
	PersistenceMode           *float64 `json:"persistence_mode,omitempty"`
	PcieLinkGeneration        *float64 `json:"pcie_link_generation,omitempty"`
	PcieLinkWidth             *float64 `json:"pcie_link_width,omitempty"`
	PcieRxBytesPerSecond      *float64 `json:"pcie_rx_bytes_per_second,omitempty"`
	PcieTxBytesPerSecond      *float64 `json:"pcie_tx_bytes_per_second,omitempty"`
	ThrottleViolationSeconds  *float64 `json:"throttle_violation_seconds,omitempty"`
	// ClocksMhz 는 clock 라벨 (sm / mem / graphics) 별 클럭이고, TemperatureThresholdsCelsius 는
	// threshold 라벨 (slowdown / shutdown / mem_max / gpu_max) 별 임계다. 서브라벨로 device 당
	// 다중 시리즈라 단일 필드가 아닌 map 으로 담는다.
	ClocksMhz                    map[string]float64 `json:"clocks_mhz,omitempty"`
	TemperatureThresholdsCelsius map[string]float64 `json:"temperature_thresholds_celsius,omitempty"`
	// ThrottleReasons 는 값이 1 (활성) 인 throttle reason 라벨 목록이다. gpu_idle 같은 정보성 사유도
	// NVML 이 주는 그대로 노출해 프론트가 필터 여부를 결정하게 한다.
	ThrottleReasons []string `json:"throttle_reasons"`
	Pods            []GpuPod `json:"pods"`
}

// GpuPod 는 한 device 를 점유 중인 pod 의 사용률과 메모리다. gpuobs 가 NVML running process 를
// cgroup 으로 Pod 귀속한 결과라 GPU 프로세스가 실행 중인 pod 만 나타난다.
type GpuPod struct {
	Namespace          string   `json:"namespace"`
	Pod                string   `json:"pod"`
	UtilizationPercent float64  `json:"utilization_percent"`
	MemoryUsedBytes    *float64 `json:"memory_used_bytes,omitempty"`
}

// gpuDeviceKey 는 device 병합 키다. gpu_uuid 가 유일키지만 표시 라벨 (node / index / model) 은 첫
// 관측값으로 채운다.
type gpuDeviceKey struct {
	node    string
	gpuUUID string
}

// gpuPodKey 는 pod 점유 병합 키다.
type gpuPodKey struct {
	gpuUUID   string
	namespace string
	pod       string
}

// GetGpuStatus godoc
// @Summary      GPU 자원 현황 조회
// @Description  node 와 GPU device 단위 사용률, 메모리, 전력, 온도, 활성 throttle reason, 점유 pod 에 더해 device 상세(SM active, encoder/decoder 사용률, bar1 메모리, 클럭, 팬, performance state, PCIe 링크, 온도 임계, throttle violation, compute/persistence mode, energy)를 한 응답으로 합성한다. gpuobs_device_* 와 gpuobs_pod_* instant query 만 쓰며 수집 공백 신호는 필드가 생략된다. SM active(gpm sm_occupancy)는 데이터센터 GPU 에서만 채워진다.
// @Tags         gpu
// @Produce      json
// @Param        node       query  string  false  "단일 노드 필터 (DNS-1123 형식, 생략 시 전체)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  GpuStatusResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/gpu-status [get]
func (h *SynthesisHandler) GetGpuStatus(w http.ResponseWriter, r *http.Request) {
	node, err := parseNodeParam(strings.TrimSpace(r.URL.Query().Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := GpuStatusResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Devices:     []GpuDevice{},
	}

	if h.querier == nil {
		resp.Summary = "GPU device 0개"
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// #263 node 필터. gpuobs_device_* / gpuobs_pod_* 는 node 라벨을 보유하므로 검증된 node 로 exact
	// 매처를 각 metric 에 붙인다. node 미지정이면 sel 이 빈 문자열이라 기존 전체 조회를 유지한다.
	sel := promSelector(nodeMatcher(node))

	// 주 소스인 사용률은 직접 조회해 실패를 500 으로 구분한다. 나머지 신호는 부가 정보라 병렬 조회
	// 후 실패 (nil) 를 필드 생략으로 graceful 처리한다.
	utils, err := h.querier.Query(ctx, "gpuobs_device_utilization_percent"+sel)
	if err != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
		return
	}

	// 단일값 device 메트릭은 (query, setter) 바인딩으로 묶어 인덱스 실수를 막는다. throttle_violation
	// 은 reason 별 다중이라 sum 으로, sm_occupancy 는 gpm 라벨로 좁혀 단일값으로 만든다. gpm 은
	// consumer GPU 에서 시리즈가 없어 자연 생략된다.
	gpmSel := promSelector(nodeMatcher(node), `gpm="sm_occupancy"`)
	binds := []struct {
		query string
		set   func(d *GpuDevice, v float64)
	}{
		{"gpuobs_device_memory_used_bytes" + sel, func(d *GpuDevice, v float64) { d.MemoryUsedBytes = &v }},
		{"gpuobs_device_memory_total_bytes" + sel, func(d *GpuDevice, v float64) { d.MemoryTotalBytes = &v }},
		{"gpuobs_device_power_usage_watts" + sel, func(d *GpuDevice, v float64) { d.PowerUsageWatts = &v }},
		{"gpuobs_device_power_limit_watts" + sel, func(d *GpuDevice, v float64) { d.PowerLimitWatts = &v }},
		{"gpuobs_device_temperature_celsius" + sel, func(d *GpuDevice, v float64) { d.TemperatureCelsius = &v }},
		{"gpuobs_device_gpm_utilization_percent" + gpmSel, func(d *GpuDevice, v float64) { d.SMActivePercent = &v }},
		{"gpuobs_device_encoder_utilization_percent" + sel, func(d *GpuDevice, v float64) { d.EncoderUtilizationPercent = &v }},
		{"gpuobs_device_decoder_utilization_percent" + sel, func(d *GpuDevice, v float64) { d.DecoderUtilizationPercent = &v }},
		{"gpuobs_device_bar1_memory_used_bytes" + sel, func(d *GpuDevice, v float64) { d.Bar1MemoryUsedBytes = &v }},
		{"gpuobs_device_bar1_memory_total_bytes" + sel, func(d *GpuDevice, v float64) { d.Bar1MemoryTotalBytes = &v }},
		{"gpuobs_device_power_limit_enforced_watts" + sel, func(d *GpuDevice, v float64) { d.PowerLimitEnforcedWatts = &v }},
		{"gpuobs_device_energy_consumption_joules_total" + sel, func(d *GpuDevice, v float64) { d.EnergyConsumptionJoules = &v }},
		{"gpuobs_device_fan_speed_percent" + sel, func(d *GpuDevice, v float64) { d.FanSpeedPercent = &v }},
		{"gpuobs_device_performance_state" + sel, func(d *GpuDevice, v float64) { d.PerformanceState = &v }},
		{"gpuobs_device_compute_mode" + sel, func(d *GpuDevice, v float64) { d.ComputeMode = &v }},
		{"gpuobs_device_persistence_mode" + sel, func(d *GpuDevice, v float64) { d.PersistenceMode = &v }},
		{"gpuobs_device_pcie_link_generation_current" + sel, func(d *GpuDevice, v float64) { d.PcieLinkGeneration = &v }},
		{"gpuobs_device_pcie_link_width_current" + sel, func(d *GpuDevice, v float64) { d.PcieLinkWidth = &v }},
		{"gpuobs_device_pcie_rx_bytes_per_second" + sel, func(d *GpuDevice, v float64) { d.PcieRxBytesPerSecond = &v }},
		{"gpuobs_device_pcie_tx_bytes_per_second" + sel, func(d *GpuDevice, v float64) { d.PcieTxBytesPerSecond = &v }},
		{fmt.Sprintf("sum by(node, gpu_uuid) (gpuobs_device_throttle_violation_seconds_total%s)", sel), func(d *GpuDevice, v float64) { d.ThrottleViolationSeconds = &v }},
	}
	bq := make([]string, len(binds))
	for i, b := range binds {
		bq[i] = b.query
	}
	bres := h.queryParallel(ctx, bq...)

	// 목록 (throttle 활성 reason) 과 서브라벨 map (clock, temperature threshold), pod 점유는 단일값이
	// 아니라 별도로 조회한다.
	sub := h.queryParallel(ctx,
		"gpuobs_device_throttle_active"+sel+" == 1",
		"gpuobs_device_clock_mhz"+sel,
		"gpuobs_device_temperature_threshold_celsius"+sel,
		"gpuobs_pod_utilization_percent"+sel,
		"gpuobs_pod_memory_used_bytes"+sel,
	)

	devices := map[gpuDeviceKey]*GpuDevice{}
	order := []gpuDeviceKey{}
	for _, sm := range utils {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		k := gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}
		if _, ok := devices[k]; !ok {
			devices[k] = &GpuDevice{
				Node:            k.node,
				GpuUUID:         k.gpuUUID,
				GpuIndex:        sm.Labels["gpu_index"],
				Model:           sm.Labels["gpu_model"],
				ThrottleReasons: []string{},
				Pods:            []GpuPod{},
			}
			order = append(order, k)
		}
		devices[k].UtilizationPercent = sm.Value
	}

	assign := func(samples []correlation.InstantSample, set func(d *GpuDevice, v float64)) {
		for _, sm := range samples {
			if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
				continue
			}
			if d, ok := devices[gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}]; ok {
				set(d, sm.Value)
			}
		}
	}
	for i, b := range binds {
		assign(bres[i], b.set)
	}

	// throttle 은 `== 1` 필터로 활성 reason 만 남으므로 라벨을 목록에 수집한다.
	for _, sm := range sub[0] {
		if d, ok := devices[gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}]; ok {
			if reason := sm.Labels["reason"]; reason != "" {
				d.ThrottleReasons = append(d.ThrottleReasons, reason)
			}
		}
	}

	// clock 과 temperature threshold 는 서브라벨 (clock, threshold) 별 다중 시리즈라 map 에 채운다.
	assignLabeled := func(samples []correlation.InstantSample, labelKey string, get func(d *GpuDevice) *map[string]float64) {
		for _, sm := range samples {
			if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
				continue
			}
			d, ok := devices[gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}]
			if !ok {
				continue
			}
			lv := sm.Labels[labelKey]
			if lv == "" {
				continue
			}
			m := get(d)
			if *m == nil {
				*m = map[string]float64{}
			}
			(*m)[lv] = sm.Value
		}
	}
	assignLabeled(sub[1], "clock", func(d *GpuDevice) *map[string]float64 { return &d.ClocksMhz })
	assignLabeled(sub[2], "threshold", func(d *GpuDevice) *map[string]float64 { return &d.TemperatureThresholdsCelsius })

	// pod 점유는 (gpu_uuid, namespace, pod) 로 병합해 utilization 과 memory 를 한 항목에 합친다.
	pods := map[gpuPodKey]*GpuPod{}
	podOwner := map[gpuPodKey]gpuDeviceKey{}
	for _, sm := range sub[3] {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		k := gpuPodKey{gpuUUID: sm.Labels["gpu_uuid"], namespace: sm.Labels["src_namespace"], pod: sm.Labels["src_pod"]}
		dk := gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}
		if _, ok := devices[dk]; !ok {
			continue
		}
		if _, ok := pods[k]; !ok {
			pods[k] = &GpuPod{Namespace: k.namespace, Pod: k.pod}
			podOwner[k] = dk
		}
		pods[k].UtilizationPercent = sm.Value
	}
	for _, sm := range sub[4] {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		k := gpuPodKey{gpuUUID: sm.Labels["gpu_uuid"], namespace: sm.Labels["src_namespace"], pod: sm.Labels["src_pod"]}
		dk := gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}
		if _, ok := devices[dk]; !ok {
			continue
		}
		if _, ok := pods[k]; !ok {
			pods[k] = &GpuPod{Namespace: k.namespace, Pod: k.pod}
			podOwner[k] = dk
		}
		v := sm.Value
		pods[k].MemoryUsedBytes = &v
	}
	for k, p := range pods {
		d := devices[podOwner[k]]
		d.Pods = append(d.Pods, *p)
	}

	for _, k := range order {
		d := devices[k]
		if d.MemoryUsedBytes != nil && d.MemoryTotalBytes != nil && *d.MemoryTotalBytes > 0 {
			ratio := *d.MemoryUsedBytes / *d.MemoryTotalBytes
			d.MemoryUsedRatio = &ratio
		}
		sort.Strings(d.ThrottleReasons)
		sort.Slice(d.Pods, func(i, j int) bool {
			if d.Pods[i].UtilizationPercent != d.Pods[j].UtilizationPercent {
				return d.Pods[i].UtilizationPercent > d.Pods[j].UtilizationPercent
			}
			if d.Pods[i].Namespace != d.Pods[j].Namespace {
				return d.Pods[i].Namespace < d.Pods[j].Namespace
			}
			return d.Pods[i].Pod < d.Pods[j].Pod
		})
		resp.Devices = append(resp.Devices, *d)
	}
	sort.Slice(resp.Devices, func(i, j int) bool {
		if resp.Devices[i].Node != resp.Devices[j].Node {
			return resp.Devices[i].Node < resp.Devices[j].Node
		}
		return resp.Devices[i].GpuIndex < resp.Devices[j].GpuIndex
	})

	resp.Summary = summarizeGpuStatus(resp)
	apicommon.WriteJSON(w, resp)
}

// summarizeGpuStatus 는 사용률 최고 device 기준 한 줄 요약을 만든다.
func summarizeGpuStatus(r GpuStatusResponse) string {
	if len(r.Devices) == 0 {
		return "GPU device 0개"
	}
	top := r.Devices[0]
	for _, d := range r.Devices[1:] {
		if d.UtilizationPercent > top.UtilizationPercent {
			top = d
		}
	}
	detail := ""
	if top.MemoryUsedRatio != nil {
		detail = fmt.Sprintf(", 메모리 %.0f%%", *top.MemoryUsedRatio*100)
	}
	return fmt.Sprintf("device %d개, 최고 사용률 %s/gpu%s %.0f%%%s, 점유 pod %d개",
		len(r.Devices), top.Node, top.GpuIndex, top.UtilizationPercent, detail, len(top.Pods))
}
