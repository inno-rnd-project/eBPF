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

// MemoryResponse 는 GET /api/v1/memory 의 typed 응답이다. pod:memory_pressure_score 가 어느 pod 인지만
// 알려주던 것을 넘어, cAdvisor 종류별 메모리(working_set / rss / cache / swap)로 분해해 "어떤 종류
// 메모리가 문제인가" 와 OOM 위험을 함께 노출한다.
type MemoryResponse struct {
	GeneratedAt string      `json:"generated_at"`
	Window      string      `json:"window"`
	Pods        []PodMemory `json:"pods"`
	Summary     string      `json:"summary"`
}

// PodMemory 는 한 pod 의 종류별 메모리 분해다. rss 는 non-reclaimable(anonymous) 로 OOM 을 직접
// 유발하고, cache 는 kernel 이 reclaim 가능해 압박이 아니다. oom_risk 는 working_set / limit 이며
// limit 미설정 pod 는 null 이다.
type PodMemory struct {
	Namespace       string   `json:"namespace"`
	Pod             string   `json:"pod"`
	Node            string   `json:"node,omitempty"`
	WorkingSetBytes float64  `json:"working_set_bytes"`
	RSSBytes        float64  `json:"rss_bytes"`
	CacheBytes      float64  `json:"cache_bytes"`
	SwapBytes       float64  `json:"swap_bytes"`
	LimitBytes      *float64 `json:"limit_bytes,omitempty"`
	OOMRisk         *float64 `json:"oom_risk,omitempty"`
	DominantKind    string   `json:"dominant_kind,omitempty"`
	Severity        string   `json:"severity"`
}

// GetMemory godoc
// @Summary      메모리 병목 분해
// @Description  pod별 cAdvisor 종류별 메모리(working_set / rss / cache / swap bytes)와 limit, OOM 위험(working_set/limit), 지배 종류를 돌려준다. rss(anonymous)는 OOM을 유발하고 cache는 reclaimable이라, working_set이 rss로 채워졌는지 cache로 채워졌는지 구분해 실제 압박 여부를 판정한다. swap은 노드 swap이 켜진 경우만 채워진다. ?namespace로 필터한다.
// @Tags         interference
// @Produce      json
// @Param        namespace  query  string  false  "namespace 필터 (생략 시 전체)"
// @Param        limit      query  int     false  "상위 N pod (1-200, 기본 30)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  MemoryResponse
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/memory [get]
func (h *SynthesisHandler) GetMemory(w http.ResponseWriter, r *http.Request) {
	nsFilter := strings.TrimSpace(r.URL.Query().Get("namespace"))
	limit := 30
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}

	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := MemoryResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Window:      "5m",
		Pods:        []PodMemory{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
		defer cancel()
		// namespace 는 fmt %q 로 이스케이프해 PromQL label matcher 로 밀어 Prometheus 측에서 필터한다.
		nsSel := ""
		if nsFilter != "" {
			nsSel = fmt.Sprintf("{namespace=%q}", nsFilter)
		}
		byPod := fmt.Sprintf("sum by(node, namespace, pod) (container_memory_%%s%s)", nsSel)
		limitSel := `{resource="memory"}`
		if nsFilter != "" {
			limitSel = fmt.Sprintf(`{resource="memory",namespace=%q}`, nsFilter)
		}
		// 주 소스 working_set 은 실패 시 돌려줄 pod 집합이 없으므로 500. 나머지 종류/limit 은
		// best-effort 로 병렬 조회한다.
		ws, err := h.querier.Query(ctx, fmt.Sprintf(byPod, "working_set_bytes"))
		if err != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
			return
		}
		rest, _ := h.queryParallel(ctx,
			fmt.Sprintf(byPod, "rss"),
			fmt.Sprintf(byPod, "cache"),
			fmt.Sprintf(byPod, "swap"),
			"sum by(namespace, pod) (kube_pod_container_resource_limits"+limitSel+")",
		)
		resp.Pods = buildPodMemory(append([][]correlation.InstantSample{ws}, rest...), limit)
	}

	resp.Summary = buildMemorySummary(resp)
	apicommon.WriteJSON(w, resp)
}

func buildPodMemory(res [][]correlation.InstantSample, limit int) []PodMemory {
	if len(res) < 5 {
		return []PodMemory{}
	}
	type podKey struct{ namespace, pod string }
	pods := map[podKey]*PodMemory{}
	get := func(ns, pod, node string) *PodMemory {
		k := podKey{ns, pod}
		p, ok := pods[k]
		if !ok {
			p = &PodMemory{Namespace: ns, Pod: pod, Node: node, Severity: "unknown"}
			pods[k] = p
		}
		if p.Node == "" {
			p.Node = node
		}
		return p
	}
	assign := func(samples []correlation.InstantSample, set func(*PodMemory, float64)) {
		for _, sm := range samples {
			if math.IsNaN(sm.Value) {
				continue
			}
			l := sm.Labels
			if l["pod"] == "" {
				continue
			}
			set(get(l["namespace"], l["pod"], l["node"]), sm.Value)
		}
	}
	assign(res[0], func(p *PodMemory, v float64) { p.WorkingSetBytes = v })
	assign(res[1], func(p *PodMemory, v float64) { p.RSSBytes = v })
	assign(res[2], func(p *PodMemory, v float64) { p.CacheBytes = v })
	assign(res[3], func(p *PodMemory, v float64) { p.SwapBytes = v })
	for _, sm := range res[4] {
		if math.IsNaN(sm.Value) || sm.Value <= 0 {
			continue
		}
		if p, ok := pods[podKey{sm.Labels["namespace"], sm.Labels["pod"]}]; ok {
			v := sm.Value
			p.LimitBytes = &v
		}
	}

	out := make([]PodMemory, 0, len(pods))
	for _, p := range pods {
		if p.LimitBytes != nil && *p.LimitBytes > 0 {
			risk := p.WorkingSetBytes / *p.LimitBytes
			p.OOMRisk = &risk
			p.Severity = correlation.PressureSeverity(risk)
		}
		p.DominantKind = dominantMemoryKind(*p)
		out = append(out, *p)
	}
	// oom_risk 있는 pod 우선(내림차순), 그다음 working_set 내림차순, 그다음 ns/pod 사전순.
	sort.Slice(out, func(i, j int) bool {
		ri, rj := oomVal(out[i]), oomVal(out[j])
		if ri != rj {
			return ri > rj
		}
		if out[i].WorkingSetBytes != out[j].WorkingSetBytes {
			return out[i].WorkingSetBytes > out[j].WorkingSetBytes
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

func oomVal(p PodMemory) float64 {
	if p.OOMRisk == nil {
		return -1
	}
	return *p.OOMRisk
}

// dominantMemoryKind 는 rss / cache / swap 중 bytes 가 가장 큰 종류를 돌려준다. 모두 0 이면 빈 문자열.
func dominantMemoryKind(p PodMemory) string {
	kind, max := "", 0.0
	for _, kv := range []struct {
		name string
		v    float64
	}{{"rss", p.RSSBytes}, {"cache", p.CacheBytes}, {"swap", p.SwapBytes}} {
		if kv.v > max {
			max, kind = kv.v, kv.name
		}
	}
	return kind
}

// buildMemorySummary 는 최고 OOM 위험 pod 와 지배 메모리 종류를 한 줄로 적는다.
func buildMemorySummary(r MemoryResponse) string {
	if len(r.Pods) == 0 {
		return "메모리 데이터 없음"
	}
	p := r.Pods[0]
	dominant := p.DominantKind
	if dominant == "" {
		dominant = "없음"
	}
	if p.OOMRisk == nil {
		return fmt.Sprintf("%s/%s working_set %.0fMB, limit 미설정 (OOM 위험 산출 불가), 지배 %s", p.Namespace, p.Pod, p.WorkingSetBytes/1e6, dominant)
	}
	return fmt.Sprintf("최고 OOM 위험 %s/%s %.0f%%(%s), 지배 메모리 %s", p.Namespace, p.Pod, *p.OOMRisk*100, p.Severity, dominant)
}
