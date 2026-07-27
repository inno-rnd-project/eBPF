package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// node-resources 는 #308 의 노드 리소스 현황 API 다. 노드 상세의 Resources 탭이 요구하는 리소스
// 종류별 capacity 와 allocatable 과 노드 내 pod requests/limits 합산과 현재 usage 와 활용률을 한
// 응답으로 합성한다. capacity/allocatable/requests/limits 는 kube-state-metrics, usage 는 cadvisor
// (cpu/memory) 와 kube_pod_info (pods) 와 gpuobs (gpu) 가 소스다.

// NodeResourcesResponse 는 GET /api/v1/node/{node}/resources 의 typed 응답이다.
type NodeResourcesResponse struct {
	GeneratedAt string `json:"generated_at"`
	Node        string `json:"node"`
	// Resources 는 리소스 종류 (cpu / memory / pods / gpu) 별 현황이다. kube-state 의 resource
	// 라벨을 매핑하며 (nvidia_com_gpu 는 gpu 키), 결측 리소스 (GPU 없는 노드 등) 는 엔트리가
	// 생략된다.
	Resources map[string]NodeResourceDetail `json:"resources"`
	Summary   string                        `json:"summary"`
}

// NodeResourceDetail 은 한 리소스 종류의 현황이다. 단위는 kube-state 원단위를 따른다 (cpu 는
// core, memory 는 byte, pods 와 gpu 는 개수). Usage 는 cpu 가 5분 rate cores, memory 가 working
// set bytes, pods 가 현재 pod 수이고, gpu 는 개수형 사용량이 없어 생략하며 활용률만 제공한다.
// UtilizationRatio 는 cpu/memory/pods 가 usage/allocatable, gpu 가 device 사용률 평균 (0-1) 이다.
type NodeResourceDetail struct {
	Capacity         *float64 `json:"capacity,omitempty"`
	Allocatable      *float64 `json:"allocatable,omitempty"`
	Requests         *float64 `json:"requests,omitempty"`
	Limits           *float64 `json:"limits,omitempty"`
	Usage            *float64 `json:"usage,omitempty"`
	UtilizationRatio *float64 `json:"utilization_ratio,omitempty"`
}

// nodeResourceKeys 는 kube-state resource 라벨을 응답 키로 매핑한다. 이슈 범위 4종만 취하고
// hugepages 와 ephemeral_storage 등은 무시한다.
var nodeResourceKeys = map[string]string{
	"cpu": "cpu", "memory": "memory", "pods": "pods", "nvidia_com_gpu": "gpu",
}

// nodeSubroute 는 /api/v1/node/ prefix 하위 경로를 세그먼트 수로 분기한다. {node} 는 노드 상세
// (GetNode), {node}/resources 는 리소스 현황 (#308), {node}/pods 는 pod 별 자원 사용량 목록
// (#330) 이다.
func (h *SynthesisHandler) nodeSubroute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/node/"), "/"), "/")
	switch {
	case len(parts) == 1:
		h.GetNode(w, r)
	case len(parts) == 2 && parts[1] == "resources":
		h.GetNodeResources(w, r)
	case len(parts) == 2 && parts[1] == "pods":
		h.GetNodePods(w, r)
	default:
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node_path", "경로는 /api/v1/node/{node} 와 /api/v1/node/{node}/resources 와 /api/v1/node/{node}/pods 형식이어야 합니다")
	}
}

// GetNodeResources godoc
// @Summary      노드 리소스 현황
// @Description  노드의 리소스 종류별 (cpu 와 memory 와 pods 와 gpu) capacity 와 allocatable, 노드 내 pod 의 requests/limits 합산, 현재 usage (cpu 는 5분 rate cores, memory 는 working set bytes, pods 는 현재 pod 수) 와 활용률 (usage/allocatable, gpu 는 device 사용률 평균 0-1) 을 한 응답으로 합성한다. 단위는 kube-state 원단위 (cpu core, memory byte, pods 와 gpu 개수) 를 따르고 결측 리소스는 엔트리가 생략된다.
// @Tags         interference
// @Produce      json
// @Param        node  path   string  true   "노드 이름 (DNS-1123 형식)"
// @Param        at    query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  NodeResourcesResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/node/{node}/resources [get]
func (h *SynthesisHandler) GetNodeResources(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/node/")
	raw := strings.TrimSuffix(strings.Trim(rest, "/"), "/resources")
	node, err := parseNodeParam(raw)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	if node == "" {
		apicommon.WriteError(w, http.StatusBadRequest, "missing_node", "경로는 /api/v1/node/{node}/resources 형식이어야 합니다")
		return
	}
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := NodeResourcesResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Node:        node,
		Resources:   map[string]NodeResourceDetail{},
	}
	if h.querier == nil {
		resp.Summary = buildNodeResourcesSummary(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// node 는 parseNodeParam 검증을 통과한 값이라 %q 결합이 안전하다. usage 의 cadvisor 합산은
	// pod-level cgroup 행 (container="", pod!="") 으로 한정한다. 이 클러스터는 pod-level 만
	// 노출되어 무필터와 동일하지만, container-level 과 root cgroup 을 함께 노출하는 표준 구성
	// 에서는 무필터 sum 이 중복 합산으로 부풀려진다 (#308 리뷰 반영, 멀티클러스터 이식성).
	sel := fmt.Sprintf("{node=%q}", node)
	podLevelSel := promSelector(nodeMatcher(node), `container=""`, `pod!=""`)
	res, qerr := h.queryParallel(ctx,
		"kube_node_status_capacity"+sel,
		"kube_node_status_allocatable"+sel,
		"sum by(resource) (kube_pod_container_resource_requests"+sel+")",
		"sum by(resource) (kube_pod_container_resource_limits"+sel+")",
		fmt.Sprintf("sum(rate(container_cpu_usage_seconds_total%s[5m]))", podLevelSel),
		"sum(container_memory_working_set_bytes"+podLevelSel+")",
		"count(kube_pod_info"+sel+")",
		"avg(gpuobs_device_utilization_percent"+sel+")",
	)
	if qerr != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
		return
	}

	// resource 라벨 시리즈 4종 (capacity / allocatable / requests / limits) 을 종류별로 채운다.
	fill := func(samples []correlation.InstantSample, set func(d *NodeResourceDetail, v float64)) {
		for _, sm := range samples {
			key, ok := nodeResourceKeys[sm.Labels["resource"]]
			if !ok || math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
				continue
			}
			d := resp.Resources[key]
			set(&d, sm.Value)
			resp.Resources[key] = d
		}
	}
	fill(res[0], func(d *NodeResourceDetail, v float64) { d.Capacity = &v })
	fill(res[1], func(d *NodeResourceDetail, v float64) { d.Allocatable = &v })
	fill(res[2], func(d *NodeResourceDetail, v float64) { d.Requests = &v })
	fill(res[3], func(d *NodeResourceDetail, v float64) { d.Limits = &v })

	// usage 와 활용률. gpu 는 개수형 usage 가 없어 사용률 평균 (0-1) 을 활용률로만 싣는다.
	setUsage := func(key string, usage *float64) {
		if usage == nil {
			return
		}
		d := resp.Resources[key]
		d.Usage = usage
		if d.Allocatable != nil && *d.Allocatable > 0 {
			ratio := *usage / *d.Allocatable
			d.UtilizationRatio = &ratio
		}
		resp.Resources[key] = d
	}
	setUsage("cpu", firstValue(res[4]))
	setUsage("memory", firstValue(res[5]))
	setUsage("pods", firstValue(res[6]))
	if v := firstValue(res[7]); v != nil {
		d := resp.Resources["gpu"]
		ratio := *v / 100
		d.UtilizationRatio = &ratio
		resp.Resources["gpu"] = d
	}

	// 어떤 시리즈에도 잡히지 않은 zero value 엔트리는 남기지 않는다 (결측 리소스 생략 규약).
	for k, d := range resp.Resources {
		if d == (NodeResourceDetail{}) {
			delete(resp.Resources, k)
		}
	}

	resp.Summary = buildNodeResourcesSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildNodeResourcesSummary 는 리소스 현황을 한 줄로 요약한다.
func buildNodeResourcesSummary(r NodeResourcesResponse) string {
	if len(r.Resources) == 0 {
		return fmt.Sprintf("노드 %s 의 리소스 데이터가 없습니다 (미존재 또는 미수집)", r.Node)
	}
	seg := fmt.Sprintf("노드 %s 리소스", r.Node)
	for _, key := range []string{"cpu", "memory", "pods", "gpu"} {
		d, ok := r.Resources[key]
		if !ok || d.UtilizationRatio == nil {
			continue
		}
		seg += fmt.Sprintf(", %s 활용률 %.0f%%", key, *d.UtilizationRatio*100)
	}
	return seg
}
