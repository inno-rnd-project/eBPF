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
// @Description  node 와 GPU device 단위 사용률, 메모리 사용량과 총량, 전력 사용량과 제한, 온도, 활성 throttle reason, device 별 점유 pod 목록을 한 응답으로 합성한다. gpuobs_device_* 와 gpuobs_pod_* instant query 만 사용하며 사용률 외 신호는 수집 공백 시 필드가 생략된다.
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

	extras := h.queryParallel(ctx,
		"gpuobs_device_memory_used_bytes"+sel,
		"gpuobs_device_memory_total_bytes"+sel,
		"gpuobs_device_power_usage_watts"+sel,
		"gpuobs_device_power_limit_watts"+sel,
		"gpuobs_device_temperature_celsius"+sel,
		"gpuobs_device_throttle_active"+sel+" == 1",
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
	assign(extras[0], func(d *GpuDevice, v float64) { d.MemoryUsedBytes = &v })
	assign(extras[1], func(d *GpuDevice, v float64) { d.MemoryTotalBytes = &v })
	assign(extras[2], func(d *GpuDevice, v float64) { d.PowerUsageWatts = &v })
	assign(extras[3], func(d *GpuDevice, v float64) { d.PowerLimitWatts = &v })
	assign(extras[4], func(d *GpuDevice, v float64) { d.TemperatureCelsius = &v })

	// throttle 은 `== 1` 필터로 활성 reason 만 남으므로 라벨을 목록에 수집한다.
	for _, sm := range extras[5] {
		if d, ok := devices[gpuDeviceKey{node: sm.Labels["node"], gpuUUID: sm.Labels["gpu_uuid"]}]; ok {
			if reason := sm.Labels["reason"]; reason != "" {
				d.ThrottleReasons = append(d.ThrottleReasons, reason)
			}
		}
	}

	// pod 점유는 (gpu_uuid, namespace, pod) 로 병합해 utilization 과 memory 를 한 항목에 합친다.
	pods := map[gpuPodKey]*GpuPod{}
	podOwner := map[gpuPodKey]gpuDeviceKey{}
	for _, sm := range extras[6] {
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
	for _, sm := range extras[7] {
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
