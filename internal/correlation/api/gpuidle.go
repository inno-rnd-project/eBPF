package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// GpuIdleResponse 는 GET /api/v1/gpu-idle 의 typed 응답이다. "GPU 가 왜 노는가" 를 노드별 유휴 비율과
// 원인 가중치 순위로 합성한다. recording rule (gpu_idle_cause_weight:5m 계열) 을 instant query 로 읽어
// Grafana 에만 있던 신호를 API 로 노출한다.
type GpuIdleResponse struct {
	GeneratedAt string              `json:"generated_at"`
	Window      string              `json:"window"`
	Scope       string              `json:"scope"`
	Nodes       []GpuNodeIdle       `json:"nodes"`
	Cluster     *GpuIdleAttribution `json:"cluster"`
	Victims     []GpuVictimIdle     `json:"victims,omitempty"`
	Summary     string              `json:"summary"`
}

// GpuNodeIdle 는 한 노드의 GPU 유휴 비율 (0-1) 과 severity 다. higher 가 더 많이 노는 worst 다.
type GpuNodeIdle struct {
	Node     string  `json:"node"`
	Idle     float64 `json:"idle"`
	Severity string  `json:"severity"`
}

// GpuIdleAttribution 은 dominant cause 와 원인별 가중치 순위다. cause weight 룰은 GPU idle > 0.5 일
// 때만 emit 되므로, 유휴 임계 미만이면 본 필드가 null 이다.
type GpuIdleAttribution struct {
	DominantCause string           `json:"dominant_cause"`
	Causes        []GpuCauseWeight `json:"causes"`
}

// GpuCauseWeight 는 한 유휴 원인의 정규화 가중치 (0-1) 다. cause 라벨은 룰이 emit 하는 값을 그대로
// 쓰므로 thermal 등 신규 원인이 룰에 추가되면 자동으로 노출된다.
type GpuCauseWeight struct {
	Cause  string  `json:"cause"`
	Weight float64 `json:"weight"`
}

// GpuVictimIdle 는 scope=pod 에서 victim Pod 단위 유휴 원인 귀속이다. pod 단위 cause set 은 node
// 단위 신호 (dcgm / nccl / thermal) 를 제외한 cluster cause 의 부분집합이다.
type GpuVictimIdle struct {
	Namespace     string           `json:"namespace"`
	Pod           string           `json:"pod"`
	Node          string           `json:"node"`
	DominantCause string           `json:"dominant_cause"`
	Causes        []GpuCauseWeight `json:"causes"`
}

// GetGpuIdle godoc
// @Summary      GPU 유휴 원인 분석
// @Description  노드별 GPU 유휴 비율과 유휴 원인 가중치 순위, dominant cause 를 합성한다. scope=cluster 는 cluster 단위 원인, scope=pod 는 victim Pod 단위 원인을 돌려준다. cause weight 는 GPU idle > 0.5 일 때만 산출되며, 미만이면 cluster 가 null 로 graceful 처리된다.
// @Tags         gpu
// @Produce      json
// @Param        scope  query  string  false  "cluster 또는 pod (기본 cluster)"
// @Param        limit  query  int     false  "scope=pod 상위 N victim (1-100, 기본 10)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  GpuIdleResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/gpu-idle [get]
func (h *SynthesisHandler) GetGpuIdle(w http.ResponseWriter, r *http.Request) {
	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("scope")))
	if scope == "" {
		scope = "cluster"
	}
	if scope != "cluster" && scope != "pod" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_scope", "scope 는 cluster 또는 pod 여야 합니다")
		return
	}
	limit := 10
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}

	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := GpuIdleResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Window:      "5m",
		Scope:       scope,
		Nodes:       []GpuNodeIdle{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
		defer cancel()

		if s, err := h.querier.Query(ctx, "node:gpu_idle:5m"); err == nil {
			for _, sm := range s {
				if math.IsNaN(sm.Value) {
					continue
				}
				resp.Nodes = append(resp.Nodes, GpuNodeIdle{
					Node:     sm.Labels["node"],
					Idle:     sm.Value,
					Severity: correlation.PressureSeverity(sm.Value),
				})
			}
			sort.Slice(resp.Nodes, func(i, j int) bool {
				if resp.Nodes[i].Idle != resp.Nodes[j].Idle {
					return resp.Nodes[i].Idle > resp.Nodes[j].Idle
				}
				return resp.Nodes[i].Node < resp.Nodes[j].Node
			})
		}

		resp.Cluster = h.clusterIdleAttribution(ctx)
		if scope == "pod" {
			resp.Victims = h.victimIdleAttribution(ctx, limit)
		}
	}

	resp.Summary = buildGpuIdleSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// clusterIdleAttribution 은 cluster 단위 원인 가중치 순위와 dominant cause 를 구한다. cause weight 가
// 없으면 (idle 게이팅 미충족) nil 을 돌려준다.
func (h *SynthesisHandler) clusterIdleAttribution(ctx context.Context) *GpuIdleAttribution {
	causes := h.causeWeights(ctx, "gpu_idle_cause_weight:5m")
	if len(causes) == 0 {
		return nil
	}
	att := &GpuIdleAttribution{Causes: causes, DominantCause: causes[0].Cause}
	if s, err := h.querier.Query(ctx, "cluster:gpu_idle_dominant_cause:5m"); err == nil && len(s) > 0 {
		if c := s[0].Labels["cause"]; c != "" {
			att.DominantCause = c
		}
	}
	return att
}

// causeWeights 는 cause 라벨이 붙은 weight 시리즈를 NaN 제외 후 weight 내림차순으로 정렬해 돌려준다.
func (h *SynthesisHandler) causeWeights(ctx context.Context, query string) []GpuCauseWeight {
	s, err := h.querier.Query(ctx, query)
	if err != nil {
		return nil
	}
	out := make([]GpuCauseWeight, 0, len(s))
	for _, sm := range s {
		if math.IsNaN(sm.Value) {
			continue
		}
		if c := sm.Labels["cause"]; c != "" {
			out = append(out, GpuCauseWeight{Cause: c, Weight: sm.Value})
		}
	}
	sortCauses(out)
	return out
}

// victimIdleAttribution 은 victim Pod 단위 원인 가중치를 (node, victim_namespace, victim_pod) 로 묶어
// dominant cause 와 함께 돌려준다. top cause weight 내림차순으로 정렬 후 limit 으로 자른다.
func (h *SynthesisHandler) victimIdleAttribution(ctx context.Context, limit int) []GpuVictimIdle {
	s, err := h.querier.Query(ctx, "pod:gpu_idle_cause_weight:5m")
	if err != nil || len(s) == 0 {
		return nil
	}
	type key struct{ node, ns, pod string }
	groups := map[key]*GpuVictimIdle{}
	order := []key{}
	for _, sm := range s {
		if math.IsNaN(sm.Value) {
			continue
		}
		cause := sm.Labels["cause"]
		if cause == "" {
			continue
		}
		k := key{sm.Labels["node"], sm.Labels["victim_namespace"], sm.Labels["victim_pod"]}
		v, ok := groups[k]
		if !ok {
			v = &GpuVictimIdle{Namespace: k.ns, Pod: k.pod, Node: k.node, Causes: []GpuCauseWeight{}}
			groups[k] = v
			order = append(order, k)
		}
		v.Causes = append(v.Causes, GpuCauseWeight{Cause: cause, Weight: sm.Value})
	}

	dom := map[key]string{}
	if ds, err := h.querier.Query(ctx, "victim:gpu_idle_dominant_cause:5m"); err == nil {
		for _, sm := range ds {
			if c := sm.Labels["cause"]; c != "" {
				dom[key{sm.Labels["node"], sm.Labels["victim_namespace"], sm.Labels["victim_pod"]}] = c
			}
		}
	}

	out := make([]GpuVictimIdle, 0, len(order))
	for _, k := range order {
		v := groups[k]
		sortCauses(v.Causes)
		if d, ok := dom[k]; ok {
			v.DominantCause = d
		} else if len(v.Causes) > 0 {
			v.DominantCause = v.Causes[0].Cause
		}
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		wi, wj := topCauseWeight(out[i].Causes), topCauseWeight(out[j].Causes)
		if wi != wj {
			return wi > wj
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Pod < out[j].Pod
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// sortCauses 는 weight 내림차순, 동률은 cause 사전순으로 정렬한다.
func sortCauses(c []GpuCauseWeight) {
	sort.Slice(c, func(i, j int) bool {
		if c[i].Weight != c[j].Weight {
			return c[i].Weight > c[j].Weight
		}
		return c[i].Cause < c[j].Cause
	})
}

// topCauseWeight 는 정렬된 cause 슬라이스의 최대 weight 를 돌려준다.
func topCauseWeight(c []GpuCauseWeight) float64 {
	if len(c) == 0 {
		return 0
	}
	return c[0].Weight
}

// buildGpuIdleSummary 는 한 줄 요약을 만든다. cause 귀속이 없으면 (idle 게이팅 미충족) 그 사실을,
// 있으면 dominant 원인과 최고 유휴 노드를 적는다.
func buildGpuIdleSummary(r GpuIdleResponse) string {
	if len(r.Nodes) == 0 {
		return "GPU 메트릭이 없어 유휴 분석 불가"
	}
	worst := r.Nodes[0]
	if r.Cluster == nil || len(r.Cluster.Causes) == 0 {
		return fmt.Sprintf("GPU 유휴 원인 귀속 임계(idle>0.5) 미만, 최고 유휴 노드 %s idle %.2f", worst.Node, worst.Idle)
	}
	top := r.Cluster.Causes[0]
	return fmt.Sprintf("GPU 유휴 dominant 원인 %s(가중치 %.2f), 최고 유휴 노드 %s idle %.2f", top.Cause, top.Weight, worst.Node, worst.Idle)
}
