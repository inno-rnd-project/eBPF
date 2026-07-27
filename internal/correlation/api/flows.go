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

// FlowsResponse 는 GET /api/v1/flows 의 typed 응답이다. netobs_flow_bytes_total(5-tuple pod 간 RX/TX)
// 을 rate 로 환산해 pod 간 대역폭 엣지로 노출한다. flow 메트릭은 NETOBS_FLOW_ALLOW_NAMESPACES allow-list
// 가 설정된 src namespace 에서만 emit 되므로, 비활성 환경에서는 edges 가 비고 flow_collection_enabled 가
// false 다.
type FlowsResponse struct {
	GeneratedAt           string     `json:"generated_at"`
	Window                string     `json:"window"`
	Edges                 []FlowEdge `json:"edges"`
	FlowCollectionEnabled bool       `json:"flow_collection_enabled"`
	// Total 은 limit 적용 전 전체 엣지 수, Truncated 는 잘렸는지다 (#352).
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Summary   string `json:"summary"`
}

// FlowEdge 는 한 pod 간 흐름 엣지다. src 는 완전 식별되고, dst 는 namespace 와 pod_uid 와 ip 로
// 식별된다 (dst_workload / dst_pod 이름은 계측에 없어 /pods 인벤토리의 uid 로 매핑한다).
type FlowEdge struct {
	Node         string  `json:"node"`
	SrcNamespace string  `json:"src_namespace,omitempty"`
	SrcWorkload  string  `json:"src_workload,omitempty"`
	SrcPod       string  `json:"src_pod,omitempty"`
	DstNamespace string  `json:"dst_namespace,omitempty"`
	DstPodUID    string  `json:"dst_pod_uid,omitempty"`
	DstIP        string  `json:"dst_ip,omitempty"`
	Direction    string  `json:"direction,omitempty"`
	Protocol     string  `json:"protocol,omitempty"`
	BytesPerSec  float64 `json:"bytes_per_sec"`
	Mbps         float64 `json:"mbps"`
}

// GetFlows godoc
// @Summary      pod 간 flow 토폴로지
// @Description  netobs_flow_bytes_total(5-tuple pod 간 RX/TX)을 rate 로 환산해 pod 간 대역폭 엣지(node·방향·protocol별 bytes/sec 와 Mbps)로 노출한다. flow 메트릭은 NETOBS_FLOW_ALLOW_NAMESPACES allow-list 가 설정된 src namespace 에서만 emit 되며, flow_collection_enabled 로 활성 여부를 알린다. dst 는 namespace·pod_uid·ip 로 식별되므로 /pods 인벤토리의 uid 와 매핑한다.
// @Tags         network
// @Produce      json
// @Param        namespace  query  string  false  "src_namespace 필터 (생략 시 전체)"
// @Param        direction  query  string  false  "egress 또는 ingress (생략 시 전체)"
// @Param        limit      query  int     false  "상위 N 엣지 (1-200, 기본 50)"
// @Success      200  {object}  FlowsResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/flows [get]
func (h *SynthesisHandler) GetFlows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nsFilter := strings.TrimSpace(q.Get("namespace"))
	direction := strings.ToLower(strings.TrimSpace(q.Get("direction")))
	if direction != "" && direction != "egress" && direction != "ingress" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_direction", "direction 은 egress 또는 ingress 여야 합니다")
		return
	}
	limit, ok := apicommon.ParseLimit(r, 50, 200)
	if !ok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit 은 정수여야 합니다")
		return
	}

	resp := FlowsResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      "5m",
		Edges:       []FlowEdge{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// direction 과 namespace 는 fmt %q (strconv.Quote) 로 이스케이프해 PromQL label matcher 로
		// 밀어 Prometheus 측에서 필터한다. injection 없이 안전하고 고카디널리티 flow 의 전송량을 줄인다.
		var matchers []string
		if direction != "" {
			matchers = append(matchers, fmt.Sprintf("direction=%q", direction))
		}
		if nsFilter != "" {
			matchers = append(matchers, fmt.Sprintf("src_namespace=%q", nsFilter))
		}
		selector := ""
		if len(matchers) > 0 {
			selector = "{" + strings.Join(matchers, ",") + "}"
		}
		query := fmt.Sprintf("sum by(node, src_namespace, src_workload, src_pod, dst_namespace, dst_pod_uid, dst_ip, protocol, direction) (rate(netobs_flow_bytes_total%s[5m]))", selector)
		s, err := h.querier.Query(ctx, query)
		if err != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
			return
		}
		edges := buildFlowEdges(s)
		resp.Total = len(edges)
		if len(edges) > limit {
			edges = edges[:limit]
			resp.Truncated = true
		}
		resp.Edges = edges
		resp.FlowCollectionEnabled = len(edges) > 0
	}

	resp.Summary = buildFlowsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

func buildFlowEdges(samples []correlation.InstantSample) []FlowEdge {
	out := []FlowEdge{}
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || sm.Value <= 0 {
			continue
		}
		l := sm.Labels
		out = append(out, FlowEdge{
			Node: l["node"], SrcNamespace: l["src_namespace"], SrcWorkload: l["src_workload"], SrcPod: l["src_pod"],
			DstNamespace: l["dst_namespace"], DstPodUID: l["dst_pod_uid"], DstIP: l["dst_ip"],
			Direction: l["direction"], Protocol: l["protocol"],
			BytesPerSec: sm.Value, Mbps: sm.Value * 8 / 1e6,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BytesPerSec != out[j].BytesPerSec {
			return out[i].BytesPerSec > out[j].BytesPerSec
		}
		if out[i].SrcNamespace != out[j].SrcNamespace {
			return out[i].SrcNamespace < out[j].SrcNamespace
		}
		if out[i].SrcPod != out[j].SrcPod {
			return out[i].SrcPod < out[j].SrcPod
		}
		return out[i].DstIP < out[j].DstIP
	})
	return out
}

// buildFlowsSummary 는 최다 대역폭 엣지와 flow 수집 활성 여부를 한 줄로 적는다.
func buildFlowsSummary(r FlowsResponse) string {
	if len(r.Edges) == 0 {
		return "flow 데이터 없음 (NETOBS_FLOW_ALLOW_NAMESPACES 활성 namespace 의 pod 간 흐름만 노출)"
	}
	e := r.Edges[0]
	return fmt.Sprintf("최다 대역폭 %s/%s → %s (%s, %.2f Mbps), 엣지 %d개", e.SrcNamespace, e.SrcPod, firstNonEmpty(e.DstNamespace, e.DstIP), e.Direction, e.Mbps, len(r.Edges))
}
