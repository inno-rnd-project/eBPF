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

// drop 분석 API 는 "어디서 무슨 reason 으로 언제 떨어졌나" 를 한 응답으로 묶는다. 항상 켜진
// netobs_drop_events_labeled_total (workload 단위) 을 주 소스로 rate 랭킹을 만들고, opt-in 인
// netobs_drop_events_flow_total (5-tuple) / netobs_drop_last_timestamp_seconds / netobs_drop_stack_total
// 은 NETOBS_DROP_FLOW_ALLOW_NAMESPACES allow-list 가 켜진 경우에만 채워지는 best-effort 필드로 둔다.

// DropsResponse 는 GET /api/v1/drops 의 typed 응답이다.
type DropsResponse struct {
	GeneratedAt string      `json:"generated_at"`
	Window      string      `json:"window"`
	Drops       []DropGroup `json:"drops"`
	Flows       []DropFlow  `json:"flows"`
	Stacks      []DropStack `json:"stacks"`
	// CiliumDrops 는 #225 의 CNI(BPF) 계층 drop 이다. kfree_skb_reason 기반 drops 가 커널 스택 drop
	// 만 잡는 사각 (NetworkPolicy 거부 등) 을 cilium_drop_count_total 합성으로 보완한다. cilium-agent
	// 단위 메트릭이라 pod 귀속이 없는 node 수준 신호다.
	CiliumDrops []CiliumDropGroup `json:"cilium_drops"`
	// Retrans 는 #226 의 workload 단위 TCP 재전송 랭킹이다. drop 과 함께 패킷 손실 계열 신호라 한
	// 응답으로 묶는다. 항상 수집되는 netobs_retrans_events_labeled_total 기반이다.
	Retrans           []RetransGroup `json:"retrans"`
	FlowDetailEnabled bool           `json:"flow_detail_enabled"`
	Summary           string         `json:"summary"`
}

// DropGroup 은 workload 단위 drop 집계다 (항상 수집). direction 별 reason / category 와 drops/sec 를 담는다.
type DropGroup struct {
	Node         string  `json:"node"`
	Namespace    string  `json:"namespace,omitempty"`
	Workload     string  `json:"workload,omitempty"`
	DstNamespace string  `json:"dst_namespace,omitempty"`
	DstWorkload  string  `json:"dst_workload,omitempty"`
	Direction    string  `json:"direction,omitempty"`
	Reason       string  `json:"reason,omitempty"`
	Category     string  `json:"category,omitempty"`
	Stage        string  `json:"stage,omitempty"`
	DropsPerSec  float64 `json:"drops_per_sec"`
}

// DropFlow 는 5-tuple 단위 drop 상세다 (opt-in). last_seen_unix 는 마지막 drop 발생 시점 (unix seconds) 이다.
type DropFlow struct {
	Node         string   `json:"node"`
	Namespace    string   `json:"namespace,omitempty"`
	Pod          string   `json:"pod,omitempty"`
	Direction    string   `json:"direction,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	Category     string   `json:"category,omitempty"`
	Protocol     string   `json:"protocol,omitempty"`
	SrcIP        string   `json:"src_ip,omitempty"`
	SrcPort      string   `json:"src_port,omitempty"`
	DstIP        string   `json:"dst_ip,omitempty"`
	DstPort      string   `json:"dst_port,omitempty"`
	IPVersion    string   `json:"ip_version,omitempty"`
	DropsPerSec  float64  `json:"drops_per_sec"`
	LastSeenUnix *float64 `json:"last_seen_unix,omitempty"`
}

// DropStack 은 커널 함수 단위 drop 집계다 (opt-in). reason / category 를 발생시킨 커널 함수 위치다.
type DropStack struct {
	Reason      string  `json:"reason,omitempty"`
	Category    string  `json:"category,omitempty"`
	Func        string  `json:"func"`
	DropsPerSec float64 `json:"drops_per_sec"`
}

// CiliumDropGroup 은 CNI(BPF) 계층 drop 의 node 단위 집계다. cilium-agent 가 emit 하는 시계열이라
// pod 귀속이 없으며, reason 은 Cilium 의 사람 읽는 사유 문자열 (Policy denied 등) 그대로다.
type CiliumDropGroup struct {
	Node        string  `json:"node"`
	Direction   string  `json:"direction,omitempty"`
	Reason      string  `json:"reason,omitempty"`
	DropsPerSec float64 `json:"drops_per_sec"`
}

// RetransGroup 은 workload 단위 TCP 재전송 집계다.
type RetransGroup struct {
	Node          string  `json:"node"`
	Namespace     string  `json:"namespace,omitempty"`
	Workload      string  `json:"workload,omitempty"`
	DstNamespace  string  `json:"dst_namespace,omitempty"`
	DstWorkload   string  `json:"dst_workload,omitempty"`
	TrafficScope  string  `json:"traffic_scope,omitempty"`
	RetransPerSec float64 `json:"retrans_per_sec"`
}

// GetDrops godoc
// @Summary      패킷 drop 분석
// @Description  netobs_drop_events_labeled_total(항상 수집) 기반으로 node·workload·reason·category·direction 별 drop rate 랭킹을 돌려준다. NETOBS_DROP_FLOW_ALLOW_NAMESPACES allow-list 가 켜진 경우 5-tuple flow(pod·src/dst ip:port·마지막 발생 시점)와 커널 stack 함수 상세가 flows/stacks 에 채워지며, flow_detail_enabled 로 활성 여부를 알린다. cilium_drops 는 CNI(BPF) 계층 drop(NetworkPolicy 거부 등, 커널 kfree_skb 경로의 사각)을 cilium_drop_count_total 로 합성한 node 수준 신호이며 pod 귀속이 없다. retrans 는 workload 단위 TCP 재전송 랭킹으로 drop 과 함께 패킷 손실 계열 신호를 한 응답으로 묶는다.
// @Tags         network
// @Produce      json
// @Param        namespace  query  string  false  "src_namespace 필터 (생략 시 전체)"
// @Param        node       query  string  false  "단일 노드 필터 (DNS-1123 형식, 생략 시 전체)"
// @Param        limit      query  int     false  "상위 N (1-100, 기본 20)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  DropsResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/drops [get]
func (h *SynthesisHandler) GetDrops(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nsFilter := strings.TrimSpace(q.Get("namespace"))
	limit := 20
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	node, err := parseNodeParam(strings.TrimSpace(q.Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	// #263 node 필터. drop / cilium / retrans 5 신호 모두 node 라벨을 보유하므로 검증된 node 로 exact
	// 매처를 rate() 안 metric selector 에 삽입한다. node 미지정이면 sel 이 빈 문자열이라 전체 조회.
	sel := promSelector(nodeMatcher(node))

	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := DropsResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Window:      "5m",
		Drops:       []DropGroup{},
		Flows:       []DropFlow{},
		Stacks:      []DropStack{},
		CiliumDrops: []CiliumDropGroup{},
		Retrans:     []RetransGroup{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
		defer cancel()

		// 주 소스 (항상 수집): 실패하면 돌려줄 핵심 데이터가 없으므로 500.
		labeled, err := h.querier.Query(ctx, fmt.Sprintf("sum by(node, src_namespace, src_workload, dst_namespace, dst_workload, direction, drop_reason, drop_category, drop_stage) (rate(netobs_drop_events_labeled_total%s[5m]))", sel))
		if err != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
			return
		}
		resp.Drops = buildDropGroups(labeled, nsFilter, limit)

		// opt-in 상세와 CNI 계층 drop: best-effort 라 실패해도 무시한다. cilium 은 미설치 클러스터에서
		// 시계열 부재로 자연히 빈 결과가 된다.
		res, qerr := h.queryParallel(ctx,
			fmt.Sprintf("sum by(node, src_namespace, src_pod, direction, drop_reason, drop_category, protocol, src_ip, src_port, dst_ip, dst_port, ip_version) (rate(netobs_drop_events_flow_total%s[5m]))", sel),
			"netobs_drop_last_timestamp_seconds"+sel,
			fmt.Sprintf("sum by(drop_reason, drop_category, func) (rate(netobs_drop_stack_total%s[5m]))", sel),
			fmt.Sprintf("sum by(node, reason, direction) (rate(cilium_drop_count_total%s[5m]))", sel),
			fmt.Sprintf("sum by(node, src_namespace, src_workload, traffic_scope, dst_namespace, dst_workload) (rate(netobs_retrans_events_labeled_total%s[5m]))", sel),
		)
		if qerr != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
			return
		}
		resp.Flows = buildDropFlows(res[0], res[1], nsFilter, limit)
		resp.Stacks = buildDropStacks(res[2], limit)
		resp.CiliumDrops = buildCiliumDrops(res[3], limit)
		resp.Retrans = buildRetransGroups(res[4], nsFilter, limit)
		resp.FlowDetailEnabled = len(resp.Flows) > 0 || len(resp.Stacks) > 0
	}

	resp.Summary = buildDropsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

func buildDropGroups(samples []correlation.InstantSample, nsFilter string, limit int) []DropGroup {
	out := []DropGroup{}
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || sm.Value <= 0 {
			continue
		}
		l := sm.Labels
		if nsFilter != "" && l["src_namespace"] != nsFilter {
			continue
		}
		out = append(out, DropGroup{
			Node: l["node"], Namespace: l["src_namespace"], Workload: l["src_workload"],
			DstNamespace: l["dst_namespace"], DstWorkload: l["dst_workload"], Direction: l["direction"],
			Reason: l["drop_reason"], Category: l["drop_category"], Stage: l["drop_stage"], DropsPerSec: sm.Value,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DropsPerSec != out[j].DropsPerSec {
			return out[i].DropsPerSec > out[j].DropsPerSec
		}
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Workload != out[j].Workload {
			return out[i].Workload < out[j].Workload
		}
		if out[i].Direction != out[j].Direction {
			return out[i].Direction < out[j].Direction
		}
		return out[i].Reason < out[j].Reason
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// fiveTupleKey 는 flow rate 와 last_timestamp 를 매칭하는 5-tuple 복합 키다. 두 메트릭이 동일 라벨
// 셋을 공유하므로 같은 키로 join 된다.
func fiveTupleKey(l map[string]string) string {
	return strings.Join([]string{l["protocol"], l["src_ip"], l["src_port"], l["dst_ip"], l["dst_port"], l["direction"], l["node"]}, "\x00")
}

func buildDropFlows(rate, lastTs []correlation.InstantSample, nsFilter string, limit int) []DropFlow {
	seen := map[string]float64{}
	for _, sm := range lastTs {
		if !math.IsNaN(sm.Value) {
			seen[fiveTupleKey(sm.Labels)] = sm.Value
		}
	}
	out := []DropFlow{}
	for _, sm := range rate {
		if math.IsNaN(sm.Value) || sm.Value <= 0 {
			continue
		}
		l := sm.Labels
		if nsFilter != "" && l["src_namespace"] != nsFilter {
			continue
		}
		f := DropFlow{
			Node: l["node"], Namespace: l["src_namespace"], Pod: l["src_pod"], Direction: l["direction"],
			Reason: l["drop_reason"], Category: l["drop_category"], Protocol: l["protocol"],
			SrcIP: l["src_ip"], SrcPort: l["src_port"], DstIP: l["dst_ip"], DstPort: l["dst_port"],
			IPVersion: l["ip_version"], DropsPerSec: sm.Value,
		}
		if ts, ok := seen[fiveTupleKey(l)]; ok {
			f.LastSeenUnix = &ts
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DropsPerSec > out[j].DropsPerSec })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func buildDropStacks(samples []correlation.InstantSample, limit int) []DropStack {
	out := []DropStack{}
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || sm.Value <= 0 {
			continue
		}
		l := sm.Labels
		out = append(out, DropStack{Reason: l["drop_reason"], Category: l["drop_category"], Func: l["func"], DropsPerSec: sm.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DropsPerSec > out[j].DropsPerSec })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// buildCiliumDrops 는 cilium_drop_count_total 의 (node, direction, reason) rate 를 내림차순 랭킹으로
// 만든다. direction 은 Cilium 이 대문자 (EGRESS / INGRESS) 로 emit 하므로 기존 필드 관례에 맞춰
// 소문자로 정규화한다.
func buildCiliumDrops(samples []correlation.InstantSample, limit int) []CiliumDropGroup {
	out := []CiliumDropGroup{}
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || sm.Value <= 0 {
			continue
		}
		out = append(out, CiliumDropGroup{
			Node:        sm.Labels["node"],
			Direction:   strings.ToLower(sm.Labels["direction"]),
			Reason:      sm.Labels["reason"],
			DropsPerSec: sm.Value,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DropsPerSec != out[j].DropsPerSec {
			return out[i].DropsPerSec > out[j].DropsPerSec
		}
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		if out[i].Direction != out[j].Direction {
			return out[i].Direction < out[j].Direction
		}
		return out[i].Reason < out[j].Reason
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// buildRetransGroups 는 netobs_retrans_events_labeled_total 의 workload 단위 rate 를 내림차순 랭킹으로
// 만든다. namespace 필터는 drops 랭킹과 동일하게 src_namespace 로 거른다.
func buildRetransGroups(samples []correlation.InstantSample, nsFilter string, limit int) []RetransGroup {
	out := []RetransGroup{}
	for _, sm := range samples {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) || sm.Value <= 0 {
			continue
		}
		l := sm.Labels
		if nsFilter != "" && l["src_namespace"] != nsFilter {
			continue
		}
		out = append(out, RetransGroup{
			Node: l["node"], Namespace: l["src_namespace"], Workload: l["src_workload"],
			DstNamespace: l["dst_namespace"], DstWorkload: l["dst_workload"],
			TrafficScope: l["traffic_scope"], RetransPerSec: sm.Value,
		})
	}
	// 쿼리 group-by 키 (node, src_*, traffic_scope, dst_*) 전체를 tie-breaker 로 써야 동률에서
	// 결정적 순서가 보장된다 (sort.Slice 는 불안정 정렬).
	sort.Slice(out, func(i, j int) bool {
		if out[i].RetransPerSec != out[j].RetransPerSec {
			return out[i].RetransPerSec > out[j].RetransPerSec
		}
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Workload != out[j].Workload {
			return out[i].Workload < out[j].Workload
		}
		if out[i].TrafficScope != out[j].TrafficScope {
			return out[i].TrafficScope < out[j].TrafficScope
		}
		if out[i].DstNamespace != out[j].DstNamespace {
			return out[i].DstNamespace < out[j].DstNamespace
		}
		return out[i].DstWorkload < out[j].DstWorkload
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// buildDropsSummary 는 최고 drop 그룹과 opt-in 상세 활성 여부를 한 줄로 적는다.
func buildDropsSummary(r DropsResponse) string {
	if len(r.Drops) == 0 {
		return "최근 5분 drop 없음"
	}
	g := r.Drops[0]
	detail := ""
	if !r.FlowDetailEnabled {
		detail = " (5-tuple·stack 상세는 NETOBS_DROP_FLOW_ALLOW_NAMESPACES 활성 시 노출)"
	}
	return fmt.Sprintf("최다 drop %s/%s node=%s reason=%s %.2f건/s%s", g.Namespace, g.Workload, g.Node, g.Reason, g.DropsPerSec, detail)
}
