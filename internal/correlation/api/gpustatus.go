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
	// DominantCauses 는 노드별 GPU 유휴 dominant cause 요약이다 (#304, node → cause). 디바이스
	// 그리드가 gpu-idle/gpu-rca 를 join 하지 않고 카드 라벨을 그릴 수 있게 한다. 원인 신호가 노드
	// 스코프라 같은 노드의 디바이스들은 동일 cause 를 공유하며, 유휴 게이팅 미충족 노드는 시리즈
	// 부재로 생략된다. 서사 (narrative) 는 gpu-rca 담당을 유지한다.
	DominantCauses map[string]GpuNodeDominantCause `json:"dominant_causes,omitempty"`
	Summary        string                          `json:"summary"`
}

// GpuNodeDominantCause 는 노드 dominant cause 의 라벨용 요약이다. description 은 gpu-rca 와 동일한
// cause 카탈로그 (rcaCauseCatalog) 에서 온다.
type GpuNodeDominantCause struct {
	Cause       string `json:"cause"`
	Description string `json:"description,omitempty"`
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
	// Status 는 device 3단 판정 (#279) 이다. 성능성 throttle 활성이면 degraded, 온도가 slowdown
	// 임계의 90% 이상이거나 노드의 유의미 NVML 오류율이 alert 임계 (1/s) 를 넘으면 warning, 그 외
	// healthy 다. #273 의 GPU health 판정 신호를 device 뱃지 입도로 적용한 것이다.
	Status string `json:"status"`
	// Idle 은 사용률이 node:gpu_idle:5m rule 과 동일한 임계 (20% 미만) 인지의 instant 판정이다
	// (#304). rule 은 5분 윈도우 비율이고 본 필드는 현재 값 판정이라 순간 부하에서는 어긋날 수 있다.
	Idle bool `json:"idle"`
	// PodAttribution 은 CUDA pod 귀속 능력 메타데이터 (#279) 다. pods 가 비었을 때 "실행 중 GPU
	// 프로세스 없음" 과 "귀속 자체가 불가능" 을 프론트가 구분하는 근거다. 심볼 신호 부재 시 생략된다.
	PodAttribution *PodAttribution `json:"pod_attribution,omitempty"`
	Pods           []GpuPod        `json:"pods"`
}

// PodAttribution 은 gpuobs_cuda_symbol_available 기반 CUDA 귀속 능력 판정이다. driver 심볼 (cu*)
// 이 필수이고 runtime 심볼 (cuda*) 은 선택이라는 규약은 GPUObsCudaSymbolUnavailable alert 와 같다.
type PodAttribution struct {
	Available      bool   `json:"available"`
	RuntimeSymbols bool   `json:"runtime_symbols"`
	Reason         string `json:"reason,omitempty"`
}

// gpuPerfThrottleReasons 는 성능성 throttle reason 셋이다. GPUObsThrottleActive alert 와 #273 GPU
// health 판정과 동일한 규약이며 정보성 gpu_idle 은 포함하지 않는다.
var gpuPerfThrottleReasons = map[string]bool{
	"hw_slowdown": true, "hw_thermal_slowdown": true, "hw_power_brake_slowdown": true,
	"sw_thermal_slowdown": true, "sw_power_cap": true,
}

// gpuIdleUtilizationThreshold 는 device idle 판정 임계 (%) 다. node:gpu_idle:5m rule 의 `< 20`
// bool 비교와 동일 값이며 rule 이 바뀌면 함께 갱신한다. Go 측 단일 진실원은 본 상수 하나다(#447).
const gpuIdleUtilizationThreshold = 20

// gpuDeviceStatus 는 device 3단 판정 (#279) 이다. 성능성 throttle 활성이면 degraded, slowdown 임계
// 의 90% 이상 온도 또는 노드 유의미 NVML 오류율 초과 (agentNvmlErrorRatePerSec, alert 와 공유) 면
// warning, 그 외 healthy 다.
func gpuDeviceStatus(d *GpuDevice, nodeNvmlRate float64) string {
	for _, r := range d.ThrottleReasons {
		if gpuPerfThrottleReasons[r] {
			return "degraded"
		}
	}
	if d.TemperatureCelsius != nil {
		if slowdown, ok := d.TemperatureThresholdsCelsius["slowdown"]; ok && slowdown > 0 && *d.TemperatureCelsius >= 0.9*slowdown {
			return "warning"
		}
	}
	if nodeNvmlRate > agentNvmlErrorRatePerSec {
		return "warning"
	}
	return "healthy"
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
// @Description  node 와 GPU device 단위 사용률, 메모리, 전력, 온도, 활성 throttle reason, 점유 pod 에 더해 device 상세(SM active, encoder/decoder 사용률, bar1 메모리, 클럭, 팬, performance state, PCIe 링크, 온도 임계, throttle violation, compute/persistence mode, energy)와 device 상태 판정(status: 성능성 throttle 은 degraded, slowdown 임계 근접 또는 노드 NVML 오류율 초과는 warning), CUDA pod 귀속 능력(pod_attribution: driver 심볼 필수·runtime 선택, 불가 사유 포함), device idle 판정(idle: node:gpu_idle:5m rule 과 동일한 사용률 20% 미만 임계의 instant 적용)과 노드별 dominant cause 요약(dominant_causes: node → cause 와 한국어 설명, gpu-rca 와 동일 카탈로그)을 한 응답으로 합성한다. 수집 공백 신호는 필드가 생략되고 SM active(gpm sm_occupancy)는 데이터센터 GPU 에서만 채워진다.
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

	// #411 단일값 device 게이지는 (node, gpu_uuid) 동일 라벨셋이라 metric 이름만 다르다. 종전에는
	// 20 개를 개별 instant 쿼리로 발사했는데 요청당 쿼리 수가 그대로 Prometheus 부하였다. __name__
	// 정규식 1 쿼리로 접고 응답의 __name__ 라벨로 setter 를 분배한다 (instant 파서가 Labels 에
	// __name__ 을 보존한다). throttle_violation 은 reason 별 다중이라 sum 으로, sm_occupancy 는 gpm
	// 라벨로 좁혀야 해 두 쿼리는 병합에서 제외한다. gpm 은 consumer GPU 에서 시리즈가 없어 자연
	// 생략된다.
	gpmSel := promSelector(nodeMatcher(node), `gpm="sm_occupancy"`)
	mergedSetters := map[string]func(d *GpuDevice, v float64){}
	binds := []struct {
		query string
		set   func(d *GpuDevice, v float64)
	}{
		{"gpuobs_device_gpm_utilization_percent" + gpmSel, func(d *GpuDevice, v float64) { d.SMActivePercent = &v }},
		{fmt.Sprintf("sum by(node, gpu_uuid) (gpuobs_device_throttle_violation_seconds_total%s)", sel), func(d *GpuDevice, v float64) { d.ThrottleViolationSeconds = &v }},
	}
	mergedSetters["gpuobs_device_memory_used_bytes"] = func(d *GpuDevice, v float64) { d.MemoryUsedBytes = &v }
	mergedSetters["gpuobs_device_memory_total_bytes"] = func(d *GpuDevice, v float64) { d.MemoryTotalBytes = &v }
	mergedSetters["gpuobs_device_power_usage_watts"] = func(d *GpuDevice, v float64) { d.PowerUsageWatts = &v }
	mergedSetters["gpuobs_device_power_limit_watts"] = func(d *GpuDevice, v float64) { d.PowerLimitWatts = &v }
	mergedSetters["gpuobs_device_temperature_celsius"] = func(d *GpuDevice, v float64) { d.TemperatureCelsius = &v }
	mergedSetters["gpuobs_device_encoder_utilization_percent"] = func(d *GpuDevice, v float64) { d.EncoderUtilizationPercent = &v }
	mergedSetters["gpuobs_device_decoder_utilization_percent"] = func(d *GpuDevice, v float64) { d.DecoderUtilizationPercent = &v }
	mergedSetters["gpuobs_device_bar1_memory_used_bytes"] = func(d *GpuDevice, v float64) { d.Bar1MemoryUsedBytes = &v }
	mergedSetters["gpuobs_device_bar1_memory_total_bytes"] = func(d *GpuDevice, v float64) { d.Bar1MemoryTotalBytes = &v }
	mergedSetters["gpuobs_device_power_limit_enforced_watts"] = func(d *GpuDevice, v float64) { d.PowerLimitEnforcedWatts = &v }
	mergedSetters["gpuobs_device_energy_consumption_joules_total"] = func(d *GpuDevice, v float64) { d.EnergyConsumptionJoules = &v }
	mergedSetters["gpuobs_device_fan_speed_percent"] = func(d *GpuDevice, v float64) { d.FanSpeedPercent = &v }
	mergedSetters["gpuobs_device_performance_state"] = func(d *GpuDevice, v float64) { d.PerformanceState = &v }
	mergedSetters["gpuobs_device_compute_mode"] = func(d *GpuDevice, v float64) { d.ComputeMode = &v }
	mergedSetters["gpuobs_device_persistence_mode"] = func(d *GpuDevice, v float64) { d.PersistenceMode = &v }
	mergedSetters["gpuobs_device_pcie_link_generation_current"] = func(d *GpuDevice, v float64) { d.PcieLinkGeneration = &v }
	mergedSetters["gpuobs_device_pcie_link_width_current"] = func(d *GpuDevice, v float64) { d.PcieLinkWidth = &v }
	mergedSetters["gpuobs_device_pcie_rx_bytes_per_second"] = func(d *GpuDevice, v float64) { d.PcieRxBytesPerSecond = &v }
	mergedSetters["gpuobs_device_pcie_tx_bytes_per_second"] = func(d *GpuDevice, v float64) { d.PcieTxBytesPerSecond = &v }

	// 병합 쿼리 1건. __name__ 정규식은 고정 리터럴 목록이라 사용자 입력이 섞이지 않고, sel 의 node
	// 매처는 parseNodeParam 검증을 통과한 값이라 결합이 안전하다.
	mergedNames := "gpuobs_device_(memory_used_bytes|memory_total_bytes|power_usage_watts|power_limit_watts|temperature_celsius|encoder_utilization_percent|decoder_utilization_percent|bar1_memory_used_bytes|bar1_memory_total_bytes|power_limit_enforced_watts|energy_consumption_joules_total|fan_speed_percent|performance_state|compute_mode|persistence_mode|pcie_link_generation_current|pcie_link_width_current|pcie_rx_bytes_per_second|pcie_tx_bytes_per_second)"
	mergedQuery := promSelector(fmt.Sprintf("__name__=~%q", mergedNames), nodeMatcher(node))
	bq := make([]string, 0, len(binds)+1)
	for _, b := range binds {
		bq = append(bq, b.query)
	}
	bq = append(bq, mergedQuery)
	bres, qerr := h.queryParallel(ctx, bq...)
	if qerr != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
		return
	}

	// 목록 (throttle 활성 reason) 과 서브라벨 map (clock, temperature threshold), pod 점유, 노드 단위
	// 능력·오류 신호 (#279) 는 단일값이 아니라 별도로 조회한다.
	sub, qerr := h.queryParallel(ctx,
		"gpuobs_device_throttle_active"+sel+" == 1",
		"gpuobs_device_clock_mhz"+sel,
		"gpuobs_device_temperature_threshold_celsius"+sel,
		"gpuobs_pod_utilization_percent"+sel,
		"gpuobs_pod_memory_used_bytes"+sel,
		"gpuobs_cuda_symbol_available"+sel,
		fmt.Sprintf(`sum by(node) (rate(gpuobs_nvml_errors_total{error_code!~"Not Supported|Not Found"%s}[5m]))`, nodeSuffixMatcher(node)),
		// #304 노드 dominant cause. tie-break 포함 rule 이라 gpu-rca 와 동일 판정 소스다.
		"node:gpu_idle_dominant_cause:5m"+sel,
	)
	if qerr != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
		return
	}

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
	// #411 병합 쿼리 결과는 __name__ 라벨로 setter 를 찾아 분배한다. 목록에 없는 이름 (정규식 확장
	// 시의 오타 등) 은 조용히 무시하지 않고 아래 assign 이 skip 하므로 필드가 비어 드러난다.
	for _, sm := range bres[len(binds)] {
		set, ok := mergedSetters[sm.Labels["__name__"]]
		if !ok {
			continue
		}
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		if d, ok := devices[gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}]; ok {
			set(d, sm.Value)
		}
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

	// #279 노드별 능력·오류 신호 집계. cuda 심볼은 (node, symbol) 시리즈라 노드별로 driver (cu*)
	// 와 runtime (cuda*) 계열의 최솟값을 모으고, NVML 유의미 오류율은 노드 단위 값이다.
	type symbolState struct{ driverOK, runtimeOK, driverSeen, runtimeSeen bool }
	symbols := map[string]*symbolState{}
	for _, sm := range sub[5] {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		n := sm.Labels["node"]
		sym := sm.Labels["symbol"]
		if n == "" || sym == "" {
			continue
		}
		st, ok := symbols[n]
		if !ok {
			st = &symbolState{driverOK: true, runtimeOK: true}
			symbols[n] = st
		}
		if strings.HasPrefix(sym, "cuda") {
			st.runtimeSeen = true
			if sm.Value != 1 {
				st.runtimeOK = false
			}
		} else {
			st.driverSeen = true
			if sm.Value != 1 {
				st.driverOK = false
			}
		}
	}
	nvmlRate := map[string]float64{}
	for _, sm := range sub[6] {
		if n := sm.Labels["node"]; n != "" && !math.IsNaN(sm.Value) {
			nvmlRate[n] = sm.Value
		}
	}

	// #304 노드별 dominant cause 요약. description 은 gpu-rca 와 동일한 카탈로그에서 온다.
	for _, sm := range sub[7] {
		n, c := sm.Labels["node"], sm.Labels["cause"]
		if n == "" || c == "" {
			continue
		}
		if resp.DominantCauses == nil {
			resp.DominantCauses = map[string]GpuNodeDominantCause{}
		}
		resp.DominantCauses[n] = GpuNodeDominantCause{Cause: c, Description: rcaCauseCatalog[c].description}
	}

	for _, k := range order {
		d := devices[k]
		if d.MemoryUsedBytes != nil && d.MemoryTotalBytes != nil && *d.MemoryTotalBytes > 0 {
			ratio := *d.MemoryUsedBytes / *d.MemoryTotalBytes
			d.MemoryUsedRatio = &ratio
		}
		d.Status = gpuDeviceStatus(d, nvmlRate[d.Node])
		d.Idle = d.UtilizationPercent < gpuIdleUtilizationThreshold
		if st, ok := symbols[d.Node]; ok && st.driverSeen {
			att := &PodAttribution{Available: st.driverOK, RuntimeSymbols: st.runtimeSeen && st.runtimeOK}
			switch {
			case !att.Available:
				att.Reason = "libcuda driver 심볼 uprobe 미부착 (ABI drift 의심) — CUDA pod 귀속 불가"
			case !att.RuntimeSymbols:
				att.Reason = "런타임 (cudart) 심볼 미부착 — driver API 계측만 수집"
			}
			d.PodAttribution = att
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
