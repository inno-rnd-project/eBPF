package api

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// GpuIdleResponse 는 GET /api/v1/gpu-idle 의 typed 응답이다. "GPU 가 왜 노는가" 를 노드별 유휴 비율과
// 원인 가중치 순위로 합성한다. recording rule (gpu_idle_cause_weight:5m 계열) 을 instant query 로 읽어
// Grafana 에만 있던 신호를 API 로 노출한다.
type GpuIdleResponse struct {
	GeneratedAt string `json:"generated_at"`
	Window      string `json:"window"`
	Scope       string `json:"scope"`
	// NodeIgnored 는 scope 가 node 가 아닌 요청에 node 파라미터가 함께 왔을 때 true 다 (#447).
	// node 필터는 scope=node 전용이라 그 외 scope 에서는 적용되지 않는데, 종전에는 무시 사실이
	// 응답에 드러나지 않아 소비자가 필터가 적용된 것으로 오독할 수 있었다.
	NodeIgnored      bool                 `json:"node_ignored,omitempty"`
	Nodes            []GpuNodeIdle        `json:"nodes"`
	Cluster          *GpuIdleAttribution  `json:"cluster"`
	NodeAttributions []GpuNodeAttribution `json:"node_attributions,omitempty"`
	Victims          []GpuVictimIdle      `json:"victims,omitempty"`
	Summary          string               `json:"summary"`
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
	// Description 은 cause 의 한국어 한 줄 설명이다 (#287 레지스트리). gpu-rca 가 채우며 gpu-idle
	// 응답에서는 생략된다.
	Description string `json:"description,omitempty"`
	// Chain 은 cause 의 단계형 인과 체인 문구다 (#303). 프론트가 narrative 파싱 없이 구조 필드로
	// 렌더할 수 있게 gpu-rca 가 채우며, gpu-idle 응답에서는 Description 과 동일하게 생략된다.
	Chain string `json:"chain,omitempty"`
}

// GpuNodeAttribution 은 scope=node 에서 노드 단위 유휴 원인 귀속이다 (#256 rule, #257 노출). node
// scope cause set 은 device 신호 (dcgm / nccl / thermal / pcie) 를 포함한 9 종으로 cluster 와 동일
// 하며, 노드별로 독립 산출된다.
type GpuNodeAttribution struct {
	Node          string           `json:"node"`
	DominantCause string           `json:"dominant_cause"`
	Causes        []GpuCauseWeight `json:"causes"`
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
// @Description  노드별 GPU 유휴 비율과 유휴 원인 가중치 순위, dominant cause 를 합성한다. scope=cluster 는 cluster 단위 원인, scope=node 는 노드 단위 원인 (node 파라미터로 단일 노드 조회), scope=pod 는 victim Pod 단위 원인을 돌려준다. cause weight 는 GPU idle > 0.5 일 때만 산출되며, 미만이면 cluster 가 null 로 graceful 처리된다.
// @Tags         gpu
// @Produce      json
// @Param        scope  query  string  false  "cluster 또는 node 또는 pod (기본 cluster)"
// @Param        node   query  string  false  "scope=node 단일 노드 필터 (DNS-1123 형식, 생략 시 전체 노드). 다른 scope 에서는 적용되지 않고 node_ignored=true 로 표기된다"
// @Param        limit  query  int     false  "scope=pod 상위 N victim (1-100, 기본 10)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  GpuIdleResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/gpu-idle [get]
func (h *SynthesisHandler) GetGpuIdle(w http.ResponseWriter, r *http.Request) {
	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("scope")))
	if scope == "" {
		scope = "cluster"
	}
	if scope != "cluster" && scope != "node" && scope != "pod" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_scope", "scope 는 cluster 또는 node 또는 pod 여야 합니다")
		return
	}
	node, err := parseNodeParam(strings.TrimSpace(r.URL.Query().Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	// #447 limit 파싱 통일. 종전에는 파싱 오류를 침묵 흡수해 기본값을 썼는데, swagger 가
	// 범위를 명시하므로 다른 목록 핸들러(flows / drops / incidents)와 동일하게 파싱 불가를
	// 400 으로 돌려준다. 범위 초과 clamp 와 기본값 정책은 ParseLimit 이 동일하게 유지한다.
	limit, lok := apicommon.ParseLimit(r, 10, 100)
	if !lok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit 은 정수여야 합니다")
		return
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

		// node 파라미터는 parseNodeParam 으로 DNS-1123 검증을 통과한 값이라 exact = 매처와 %q 결합이
		// 안전하다. node 필터는 scope=node 전용이다. scope=cluster/pod 에서도 적용하면 공통 필드인
		// resp.Nodes 만 한 노드로 좁혀지고 Cluster/Victims 는 전체 기준이라 한 응답 안에서 불일치가
		// 생기므로 scope 를 함께 가드한다. 빈 값이면 selector 없이 전체 노드를 조회한다.
		nodeSelector := ""
		if scope == "node" && node != "" {
			nodeSelector = fmt.Sprintf("{node=%q}", node)
		} else if node != "" {
			// #447 무시 사실을 응답에 표기한다. 필터 미적용 조회 자체는 종전과 동일하다.
			resp.NodeIgnored = true
		}

		// #404 필수/부가 분리. 종전에는 전 쿼리를 err == nil 조건에 흡수해 백엔드 전면 장애가 200 과
		// 빈 응답으로 숨었다. 유휴 판정 (node:gpu_idle:5m) 과 요청 scope 의 cause weight 는 응답의
		// 본체라 필수 (실패 시 500 query_failed) 고, dominant cause 조회는 weight 1위 fallback 이
		// 있어 부가 (queryParallelOptional, 실패 시 log degrade) 다.
		reqQueries := []string{"node:gpu_idle:5m" + nodeSelector, "gpu_idle_cause_weight:5m"}
		scopeWeightIdx := -1
		switch scope {
		case "node":
			scopeWeightIdx = len(reqQueries)
			reqQueries = append(reqQueries, "node:gpu_idle_cause_weight:5m"+nodeSelector)
		case "pod":
			scopeWeightIdx = len(reqQueries)
			reqQueries = append(reqQueries, "pod:gpu_idle_cause_weight:5m")
		}
		reqRes, qerr := h.queryParallel(ctx, reqQueries...)
		if qerr != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
			return
		}

		optQueries := []string{"cluster:gpu_idle_dominant_cause:5m"}
		switch scope {
		case "node":
			optQueries = append(optQueries, "node:gpu_idle_dominant_cause:5m"+nodeSelector)
		case "pod":
			optQueries = append(optQueries, "victim:gpu_idle_dominant_cause:5m")
		}
		optRes, optFailed := h.queryParallelOptional(ctx, optQueries...)
		if optFailed > 0 {
			log.Printf("gpu-idle: dominant 조회 %d개 실패, weight 1위 fallback 으로 degrade", optFailed)
		}

		for _, sm := range reqRes[0] {
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

		resp.Cluster = clusterIdleAttributionFrom(reqRes[1], optRes[0])
		switch scope {
		case "node":
			resp.NodeAttributions = nodeIdleAttributionFrom(reqRes[scopeWeightIdx], optRes[1])
		case "pod":
			resp.Victims = victimIdleAttributionFrom(reqRes[scopeWeightIdx], optRes[1], limit)
		}
	}

	resp.Summary = buildGpuIdleSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// clusterIdleAttributionFrom 은 cluster 단위 원인 가중치 순위와 dominant cause 를 사전 조회된
// 샘플에서 구한다 (#404, 쿼리는 핸들러의 필수/부가 라운드가 수행). cause weight 가 없으면 (idle
// 게이팅 미충족) nil 을 돌려준다.
func clusterIdleAttributionFrom(weights, dom []correlation.InstantSample) *GpuIdleAttribution {
	causes := causeWeightsFrom(weights)
	if len(causes) == 0 {
		return nil
	}
	att := &GpuIdleAttribution{Causes: causes, DominantCause: causes[0].Cause}
	if len(dom) > 0 {
		if c := dom[0].Labels["cause"]; c != "" {
			att.DominantCause = c
		}
	}
	return att
}

// causeWeightsFrom 은 cause 라벨이 붙은 weight 시리즈를 NaN 제외 후 weight 내림차순으로 정렬해 돌려준다.
func causeWeightsFrom(s []correlation.InstantSample) []GpuCauseWeight {
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

// victimIdleAttributionFrom 은 victim Pod 단위 원인 가중치를 (node, victim_namespace, victim_pod) 로
// 묶어 dominant cause 와 함께 사전 조회된 샘플에서 구한다 (#404). top cause weight 내림차순으로 정렬
// 후 limit 으로 자른다.
func victimIdleAttributionFrom(s, ds []correlation.InstantSample, limit int) []GpuVictimIdle {
	if len(s) == 0 {
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
	for _, sm := range ds {
		if c := sm.Labels["cause"]; c != "" {
			dom[key{sm.Labels["node"], sm.Labels["victim_namespace"], sm.Labels["victim_pod"]}] = c
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

// nodeIdleAttributionFrom 은 node 단위 원인 가중치를 node 별로 묶어 dominant cause 와 함께 사전
// 조회된 샘플에서 구한다 (#404)
// (#256 rule, #257 노출). nodeSelector 는 parseNodeParam 검증을 통과한 exact 매처 (또는 빈 문자열)
// 라 안전하다. top cause weight 내림차순, 동률은 node 이름 사전순으로 정렬한다.
func nodeIdleAttributionFrom(s, ds []correlation.InstantSample) []GpuNodeAttribution {
	if len(s) == 0 {
		return nil
	}
	groups := map[string]*GpuNodeAttribution{}
	order := []string{}
	for _, sm := range s {
		if math.IsNaN(sm.Value) {
			continue
		}
		cause := sm.Labels["cause"]
		node := sm.Labels["node"]
		if cause == "" || node == "" {
			continue
		}
		v, ok := groups[node]
		if !ok {
			v = &GpuNodeAttribution{Node: node, Causes: []GpuCauseWeight{}}
			groups[node] = v
			order = append(order, node)
		}
		v.Causes = append(v.Causes, GpuCauseWeight{Cause: cause, Weight: sm.Value})
	}

	dom := map[string]string{}
	for _, sm := range ds {
		if c := sm.Labels["cause"]; c != "" {
			dom[sm.Labels["node"]] = c
		}
	}

	out := make([]GpuNodeAttribution, 0, len(order))
	for _, node := range order {
		v := groups[node]
		sortCauses(v.Causes)
		if d, ok := dom[node]; ok {
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
		return out[i].Node < out[j].Node
	})
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
