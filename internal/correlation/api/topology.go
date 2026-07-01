package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// TopologyResponse 는 GET /api/v1/topology 의 typed 응답이다. 노드별 status 와 노드간 간섭 엣지를 한
// 응답으로 합성해 대시보드가 /nodes 와 /cross-node-interference 와 pressure query 를 각각 호출해 조합
// 하던 비용을 없앤다.
type TopologyResponse struct {
	GeneratedAt string         `json:"generated_at"`
	Window      string         `json:"window"`
	Nodes       []TopologyNode `json:"nodes"`
	Edges       []TopologyEdge `json:"edges"`
	Summary     string         `json:"summary"`
}

// TopologyNode 는 한 노드의 status 와 차원별 pressure 다. status 는 dominant 차원 severity 기준
// healthy / warning / critical / unknown 4단계다.
type TopologyNode struct {
	Node              string             `json:"node"`
	Status            string             `json:"status"`
	DominantDimension string             `json:"dominant_dimension,omitempty"`
	Pressure          map[string]float64 `json:"pressure"`
}

// TopologyEdge 는 노드간 간섭 엣지다. suspect_node 가 victim_node 에 dimension 자원으로 영향을 준다.
type TopologyEdge struct {
	SuspectNode string  `json:"suspect_node"`
	VictimNode  string  `json:"victim_node"`
	Dimension   string  `json:"dimension"`
	Score       float64 `json:"score"`
}

// GetTopology godoc
// @Summary      클러스터 노드 토폴로지
// @Description  노드별 status(healthy/warning/critical/unknown, 4개 자원 차원 pressure 의 dominant severity 기준)와 노드간 간섭 엣지(cross-node interference 의 suspect_node→victim_node)를 한 응답으로 합성한다. querier 나 cross-node snapshot 이 없으면 해당 부분을 빈 값으로 graceful 처리한다.
// @Tags         synthesis
// @Produce      json
// @Success      200  {object}  TopologyResponse
// @Router       /api/v1/topology [get]
func (h *SynthesisHandler) GetTopology(w http.ResponseWriter, r *http.Request) {
	resp := TopologyResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      "5m",
		Nodes:       []TopologyNode{},
		Edges:       []TopologyEdge{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		queries := make([]string, len(synthDimensions))
		for i, d := range synthDimensions {
			queries[i] = d.nodePressure
		}
		res := h.queryParallel(ctx, queries...)
		resp.Nodes = buildTopologyNodes(res)
	}

	if h.crossNode != nil {
		for _, ni := range h.crossNode.CrossNodeSnapshot() {
			if math.IsNaN(ni.Score) {
				continue
			}
			resp.Edges = append(resp.Edges, TopologyEdge{
				SuspectNode: ni.SuspectNode, VictimNode: ni.VictimNode,
				Dimension: string(ni.Dimension), Score: ni.Score,
			})
		}
	}

	resp.Summary = buildTopologySummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildTopologyNodes 는 차원별 node pressure 벡터 (synthDimensions 순서) 를 노드별로 묶어 status 와
// dominant 차원을 판정한다.
func buildTopologyNodes(res [][]correlation.InstantSample) []TopologyNode {
	nodes := map[string]*TopologyNode{}
	get := func(name string) *TopologyNode {
		n, ok := nodes[name]
		if !ok {
			n = &TopologyNode{Node: name, Status: "unknown", Pressure: map[string]float64{}}
			nodes[name] = n
		}
		return n
	}
	for i := range res {
		if i >= len(synthDimensions) {
			break
		}
		dim := synthDimensions[i].name
		for _, sm := range res[i] {
			name := sm.Labels["node"]
			if name == "" || math.IsNaN(sm.Value) {
				continue
			}
			get(name).Pressure[dim] = sm.Value
		}
	}

	out := make([]TopologyNode, 0, len(nodes))
	for _, n := range nodes {
		domDim, domVal := "", math.Inf(-1)
		for dim, v := range n.Pressure {
			if v > domVal {
				domVal, domDim = v, dim
			}
		}
		if domDim != "" {
			n.DominantDimension = domDim
			n.Status = topologyStatus(domVal)
		}
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// topologyStatus 는 dominant pressure score 를 healthy / warning / critical / unknown 으로 환산한다.
func topologyStatus(p float64) string {
	switch correlation.PressureSeverity(p) {
	case "high":
		return "critical"
	case "elevated":
		return "warning"
	case "low":
		return "healthy"
	default:
		return "unknown"
	}
}

// buildTopologySummary 는 노드 status 분포와 엣지 수를 한 줄로 적는다.
func buildTopologySummary(r TopologyResponse) string {
	if len(r.Nodes) == 0 {
		return "노드 pressure 데이터 없음"
	}
	crit, warn := 0, 0
	for _, n := range r.Nodes {
		switch n.Status {
		case "critical":
			crit++
		case "warning":
			warn++
		}
	}
	return fmt.Sprintf("노드 %d개 (critical %d, warning %d), 간섭 엣지 %d개", len(r.Nodes), crit, warn, len(r.Edges))
}
