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

// LatencyBreakdownResponse 는 GET /api/v1/latency-breakdown 의 typed 응답이다. "이 워크로드/노드/파드의
// 지연이 커널 어느 단계에서 나는가" 를 단계별 p99 와 비중, 지배 단계로 분해한다. p99 는 백분위라 단계
// 합이 정확한 총지연은 아니며, share 는 단계 p99 / 단계 p99 합으로 상대 기여를 보이는 휴리스틱이다.
type LatencyBreakdownResponse struct {
	GeneratedAt string          `json:"generated_at"`
	Window      string          `json:"window"`
	Scope       string          `json:"scope"`
	Direction   string          `json:"direction,omitempty"`
	Targets     []LatencyTarget `json:"targets"`
	Summary     string          `json:"summary"`
}

// LatencyTarget 은 한 대상 (scope 에 따라 workload / node / pod) 의 단계 분해다.
type LatencyTarget struct {
	Namespace     string         `json:"namespace,omitempty"`
	Workload      string         `json:"workload,omitempty"`
	Pod           string         `json:"pod,omitempty"`
	Node          string         `json:"node,omitempty"`
	DominantStage string         `json:"dominant_stage"`
	Stages        []StageLatency `json:"stages"`
	// TcpState 는 #226 의 수신 pod TCP 상태 (srtt 와 cwnd) 다. netobs_tcp_state_* 가 pod 단위 라벨이라
	// scope=pod 에서만 join 되고 workload / node scope 에서는 생략된다. srtt 는 경로 RTT 신호라 커널
	// 단계 처리 지연 (stages) 과 구분해 병목이 경로인지 처리인지 가른다.
	TcpState *TcpStateInfo `json:"tcp_state,omitempty"`
}

// StageLatency 는 한 커널 단계의 p99 와 비중이다.
type StageLatency struct {
	Stage      string  `json:"stage"`
	P99Seconds float64 `json:"p99_seconds"`
	Share      float64 `json:"share"`
}

// TcpStateInfo 는 수신 pod 의 TCP 상태 요약이다. netobs 가 receive path 에서 집계한 srtt 최대값과
// congestion window 최소값으로, 값이 수집 공백이면 필드가 생략된다.
type TcpStateInfo struct {
	MaxSrttSeconds *float64 `json:"max_srtt_seconds,omitempty"`
	MinCwnd        *float64 `json:"min_cwnd,omitempty"`
}

// latencyScope 는 scope 별 히스토그램 메트릭과 그룹 라벨, 대상 식별 라벨을 묶는다.
type latencyScope struct {
	metric   string
	byLabels string
	idLabels []string // 대상 식별 라벨 (stage 제외)
}

var latencyScopes = map[string]latencyScope{
	"workload": {"netobs_stage_latency_labeled_seconds_bucket", "src_namespace, src_workload, stage, le", []string{"src_namespace", "src_workload"}},
	"node":     {"netobs_stage_latency_labeled_seconds_bucket", "node, stage, le", []string{"node"}},
	"pod":      {"netobs_pod_stage_latency_labeled_seconds_bucket", "src_namespace, src_pod, stage, le", []string{"src_namespace", "src_pod"}},
}

// GetLatencyBreakdown godoc
// @Summary      지연 단계 분해
// @Description  송신/수신 커널 단계별 p99 latency 와 비중, 지배 단계를 scope(workload/node/pod)별로 분해한다. histogram_quantile 로 stage 라벨을 보존해 산출하며, pod scope 는 send-path 단계만 수집된다. direction(egress/ingress)으로 송신/수신을 좁힐 수 있다.
// @Tags         network
// @Produce      json
// @Param        scope      query  string  false  "workload / node / pod (기본 workload)"
// @Param        direction  query  string  false  "egress 또는 ingress (생략 시 전체)"
// @Param        limit      query  int     false  "상위 N 대상 (1-100, 기본 10)"
// @Success      200  {object}  LatencyBreakdownResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/latency-breakdown [get]
func (h *SynthesisHandler) GetLatencyBreakdown(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := strings.ToLower(strings.TrimSpace(q.Get("scope")))
	if scope == "" {
		scope = "workload"
	}
	sc, ok := latencyScopes[scope]
	if !ok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_scope", "scope 는 workload / node / pod 중 하나여야 합니다")
		return
	}
	// direction 은 PromQL injection 을 피해 고정 리터럴만 허용한다.
	direction := strings.ToLower(strings.TrimSpace(q.Get("direction")))
	if direction != "" && direction != "egress" && direction != "ingress" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_direction", "direction 은 egress 또는 ingress 여야 합니다")
		return
	}
	limit := 10
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}

	resp := LatencyBreakdownResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      "5m",
		Scope:       scope,
		Direction:   direction,
		Targets:     []LatencyTarget{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		selector := ""
		if direction != "" {
			selector = fmt.Sprintf("{direction=%q}", direction)
		}
		query := fmt.Sprintf("histogram_quantile(0.99, sum by(%s) (rate(%s%s[5m])))", sc.byLabels, sc.metric, selector)
		s, err := h.querier.Query(ctx, query)
		if err != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
			return
		}
		resp.Targets = groupLatencyTargets(s, sc, limit)

		// #226 pod scope 한정 TCP 상태 join. netobs_tcp_state_* 가 (namespace, pod) 단위 라벨이라
		// workload / node scope 에는 붙이지 않는다. best-effort 병렬 조회라 실패 시 필드 생략.
		if scope == "pod" {
			st := h.queryParallel(ctx,
				"max by(namespace, pod) (netobs_tcp_state_max_srtt_seconds)",
				"min by(namespace, pod) (netobs_tcp_state_min_cwnd)",
			)
			attachTcpState(resp.Targets, st[0], st[1])
		}
	}

	resp.Summary = buildLatencySummary(resp)
	apicommon.WriteJSON(w, resp)
}

// attachTcpState 는 pod scope 타겟에 (namespace, pod) 키로 TCP 상태를 join 한다. NaN / Inf 샘플은
// JSON 직렬화 불가라 배제하고, 두 신호 모두 없는 pod 는 TcpState 자체를 생략한다.
func attachTcpState(targets []LatencyTarget, srtt, cwnd []correlation.InstantSample) {
	type podKey struct{ ns, pod string }
	srttBy := map[podKey]float64{}
	for _, sm := range srtt {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		srttBy[podKey{sm.Labels["namespace"], sm.Labels["pod"]}] = sm.Value
	}
	cwndBy := map[podKey]float64{}
	for _, sm := range cwnd {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		cwndBy[podKey{sm.Labels["namespace"], sm.Labels["pod"]}] = sm.Value
	}
	for i := range targets {
		k := podKey{targets[i].Namespace, targets[i].Pod}
		info := &TcpStateInfo{}
		if v, ok := srttBy[k]; ok {
			info.MaxSrttSeconds = &v
		}
		if v, ok := cwndBy[k]; ok {
			info.MinCwnd = &v
		}
		if info.MaxSrttSeconds != nil || info.MinCwnd != nil {
			targets[i].TcpState = info
		}
	}
}

// groupLatencyTargets 는 (대상, stage) p99 벡터를 대상별로 묶어 단계 분해를 만든다. 대상은 worst stage
// p99 내림차순, 동률은 식별 라벨 사전순으로 정렬 후 limit 으로 자른다.
func groupLatencyTargets(samples []correlation.InstantSample, sc latencyScope, limit int) []LatencyTarget {
	type agg struct {
		t      *LatencyTarget
		key    string
		stages []StageLatency
	}
	groups := map[string]*agg{}
	order := []string{}
	for _, sm := range samples {
		labels := sm.Labels
		stage := labels["stage"]
		val := sm.Value
		if stage == "" || math.IsNaN(val) {
			continue
		}
		key := labels[sc.idLabels[0]]
		if len(sc.idLabels) > 1 {
			key += "\x00" + labels[sc.idLabels[1]]
		}
		g, ok := groups[key]
		if !ok {
			g = &agg{t: newLatencyTarget(labels, sc), key: key}
			groups[key] = g
			order = append(order, key)
		}
		g.stages = append(g.stages, StageLatency{Stage: stage, P99Seconds: val})
	}

	out := make([]LatencyTarget, 0, len(order))
	for _, key := range order {
		g := groups[key]
		var sum float64
		for _, st := range g.stages {
			sum += st.P99Seconds
		}
		for i := range g.stages {
			if sum > 0 {
				g.stages[i].Share = g.stages[i].P99Seconds / sum
			}
		}
		sort.Slice(g.stages, func(i, j int) bool {
			if g.stages[i].P99Seconds != g.stages[j].P99Seconds {
				return g.stages[i].P99Seconds > g.stages[j].P99Seconds
			}
			return g.stages[i].Stage < g.stages[j].Stage
		})
		g.t.Stages = g.stages
		if len(g.stages) > 0 {
			g.t.DominantStage = g.stages[0].Stage
		}
		out = append(out, *g.t)
	}
	sort.Slice(out, func(i, j int) bool {
		wi, wj := topStageP99(out[i].Stages), topStageP99(out[j].Stages)
		if wi != wj {
			return wi > wj
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Workload != out[j].Workload {
			return out[i].Workload < out[j].Workload
		}
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].Node < out[j].Node
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func newLatencyTarget(labels map[string]string, sc latencyScope) *LatencyTarget {
	t := &LatencyTarget{Stages: []StageLatency{}}
	switch sc.idLabels[0] {
	case "node":
		t.Node = labels["node"]
	default:
		t.Namespace = labels["src_namespace"]
		if sc.idLabels[1] == "src_pod" {
			t.Pod = labels["src_pod"]
		} else {
			t.Workload = labels["src_workload"]
		}
	}
	return t
}

func topStageP99(s []StageLatency) float64 {
	if len(s) == 0 {
		return 0
	}
	return s[0].P99Seconds
}

// buildLatencySummary 는 최고 지연 대상과 그 지배 단계를 한 줄로 적는다.
func buildLatencySummary(r LatencyBreakdownResponse) string {
	if len(r.Targets) == 0 {
		return "지연 단계 데이터 없음"
	}
	t := r.Targets[0]
	name := t.Node
	if name == "" {
		name = t.Namespace + "/" + firstNonEmpty(t.Workload, t.Pod)
	}
	return fmt.Sprintf("최고 지연 대상 %s 의 지배 단계 %s (p99 %.3fms)", name, t.DominantStage, topStageP99(t.Stages)*1e3)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
