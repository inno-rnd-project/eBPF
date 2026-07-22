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

// node-vitals 는 #265 의 노드 raw 사용률 프록시 API 다. 압박 score 가 아니라 cadvisor 와 gpuobs 의
// 원시 사용률 퍼센트 (노드 allocatable 대비 pod 합산 CPU 와 memory 점유율, GPU 사용률과 메모리) 를
// instant query 로 읽어 노드 단위로 집계한다. #313 에서 분모를 pod limit 에서 노드 allocatable 로
// 교체했다. limit 분모는 limit 없는 pod 를 계산에서 제외해 control-plane (CPU limit 보유 pod 0개)
// 에서 빈 값이 되고 일반 노드도 실사용률과 괴리됐으며, limit 근접도 의미는 pressure score 계열이
// 담당한다. 정제나 score 변환 없이 raw 사용률을 노출하며, 프론트가 Prometheus 를 직접 쿼리하지
// 않고 단일 API 경로로 Vitals 와 실시간 폴링을 받게 한다. 시계열 range 는 trends 가, 압박 score
// 는 pressure 와 노드 health 가 담당한다.

// NodeVitalsResponse 는 GET /api/v1/node-vitals 의 typed 응답이다. 각 사용률은 수집 공백 시 생략되도록
// pointer 로 둔다 (NaN 은 JSON 직렬화 불가라 omitempty pointer 규약).
type NodeVitalsResponse struct {
	GeneratedAt        string   `json:"generated_at"`
	Node               string   `json:"node"`
	CPUPercent         *float64 `json:"cpu_percent,omitempty"`
	MemoryPercent      *float64 `json:"memory_percent,omitempty"`
	GPUPercent         *float64 `json:"gpu_percent,omitempty"`
	GPUMemoryUsedBytes *float64 `json:"gpu_memory_used_bytes,omitempty"`
	GPUMemoryTotalBytes *float64 `json:"gpu_memory_total_bytes,omitempty"`
	GPUMemoryPercent   *float64 `json:"gpu_memory_percent,omitempty"`
	Summary            string   `json:"summary"`
}

// GetNodeVitals godoc
// @Summary      노드 raw 사용률 (Vitals)
// @Description  노드의 CPU 와 memory 사용률 퍼센트 (노드 내 pod 합산 사용량을 노드 allocatable 로 나눈 점유율, #313), GPU 사용률과 GPU 메모리를 instant query 로 읽어 노드 단위로 돌려준다. cadvisor 와 gpuobs 원시 게이지 기반이며 압박 score 로 변환하지 않는다. limit 근접도 의미는 pressure 계열이 담당한다. 프론트가 주기 폴링으로 실시간 값을 받는 경로이며, 시계열 range 는 trends 가 담당한다.
// @Tags         interference
// @Produce      json
// @Param        node  query  string  true   "대상 노드 (DNS-1123 형식)"
// @Param        at    query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  NodeVitalsResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/node-vitals [get]
func (h *SynthesisHandler) GetNodeVitals(w http.ResponseWriter, r *http.Request) {
	node, err := parseNodeParam(strings.TrimSpace(r.URL.Query().Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	if node == "" {
		apicommon.WriteError(w, http.StatusBadRequest, "missing_node", "node 파라미터가 필요합니다")
		return
	}
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := NodeVitalsResponse{GeneratedAt: evalAt.Format(time.RFC3339), Node: node}
	if h.querier == nil {
		resp.Summary = buildNodeVitalsSummary(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// node 는 parseNodeParam 검증을 통과한 값이라 %q 결합이 안전하다. CPU 와 memory 는 노드 내
	// pod 합산 사용량을 노드 allocatable 로 나눈 점유율이다 (#313, limit 분모의 커버리지 공백 제거).
	// 분자 합산은 pod-level cgroup 행 (container="", pod!="") 으로 한정해 container-level 과 root
	// cgroup 을 함께 노출하는 표준 cadvisor 구성의 중복 합산을 막는다 (node-resources 와 동일 규약).
	// GPU 는 device 사용률 노드 평균, GPU 메모리는 노드 device used/total 합이다.
	podLevelSel := promSelector(nodeMatcher(node), `container=""`, `pod!=""`)
	res := h.queryParallel(ctx,
		fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total%s[5m])) / sum(kube_node_status_allocatable{node=%q, resource="cpu"}) * 100`, podLevelSel, node),
		fmt.Sprintf(`sum(container_memory_working_set_bytes%s) / sum(kube_node_status_allocatable{node=%q, resource="memory"}) * 100`, podLevelSel, node),
		fmt.Sprintf(`avg by(node) (gpuobs_device_utilization_percent{node=%q})`, node),
		fmt.Sprintf(`sum by(node) (gpuobs_device_memory_used_bytes{node=%q})`, node),
		fmt.Sprintf(`sum by(node) (gpuobs_device_memory_total_bytes{node=%q})`, node),
	)
	resp.CPUPercent = firstValue(res[0])
	resp.MemoryPercent = firstValue(res[1])
	resp.GPUPercent = firstValue(res[2])
	resp.GPUMemoryUsedBytes = firstValue(res[3])
	resp.GPUMemoryTotalBytes = firstValue(res[4])
	if resp.GPUMemoryUsedBytes != nil && resp.GPUMemoryTotalBytes != nil && *resp.GPUMemoryTotalBytes > 0 {
		pct := *resp.GPUMemoryUsedBytes / *resp.GPUMemoryTotalBytes * 100
		resp.GPUMemoryPercent = &pct
	}

	resp.Summary = buildNodeVitalsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// firstValue 는 instant 결과의 첫 샘플 값을 pointer 로 돌려준다. 결과가 비었거나 NaN/Inf 면 nil 이라
// 응답에서 생략된다.
func firstValue(samples []correlation.InstantSample) *float64 {
	if len(samples) == 0 || math.IsNaN(samples[0].Value) || math.IsInf(samples[0].Value, 0) {
		return nil
	}
	v := samples[0].Value
	return &v
}

// buildNodeVitalsSummary 는 사용률을 한 줄로 요약한다.
func buildNodeVitalsSummary(r NodeVitalsResponse) string {
	seg := fmt.Sprintf("노드 %s 사용률", r.Node)
	seg += " CPU " + pctText(r.CPUPercent)
	seg += ", Memory " + pctText(r.MemoryPercent)
	if r.GPUPercent != nil {
		seg += ", GPU " + pctText(r.GPUPercent)
	}
	if r.GPUMemoryPercent != nil {
		seg += ", GPU 메모리 " + pctText(r.GPUMemoryPercent)
	}
	return seg
}

// pctText 는 퍼센트 pointer 를 표시 문자열로 만든다. nil 은 데이터 부재라 "-" 다.
func pctText(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *v)
}
