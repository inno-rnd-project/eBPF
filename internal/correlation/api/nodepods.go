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

// node/{node}/pods 는 #330 의 노드 스코프 pod 단위 자원 사용량 목록이다. 노드 상세의 pod 표를 한
// 호출로 채우는 표면으로, pods API (전 클러스터 경량 인벤토리) 와 pod 상세 API (단일 pod 심층) 의
// 중간 입도를 담당한다. 쿼리는 pod 당 반복이 아니라 node 필터 + sum by(namespace, pod) 집계의
// 고정 개수로 구성된다.

// NodePodsResponse 는 GET /api/v1/node/{node}/pods 의 typed 응답이다. pods 는 namespace 와 pod
// 사전순의 결정적 정렬이며, 이상 우선 정렬은 severity 필드를 근거로 표시 계층이 수행한다.
type NodePodsResponse struct {
	GeneratedAt string         `json:"generated_at"`
	Node        string         `json:"node"`
	Pods        []NodePodUsage `json:"pods"`
	Summary     string         `json:"summary"`
}

// NodePodUsage 는 노드 위 한 pod 의 자원 사용량과 상태다. percent 2종은 limit 분모라 limit 미설정
// pod 는 생략되고 절대량 (cores / working set bytes) 으로 표현된다 (pod 상세 vitals 와 동일 규약,
// #328). severity 는 pod pressure score 3종 최대의 PressureSeverity 환산 (low/elevated/high) 이며
// score 미산출 pod 는 생략된다. unobserved_reason 은 pods API 와 동일 enum·생략 조건 (#320) 이되,
// 관측 판정 소스가 rate[5m] 결과 존재라 (pods API 는 시리즈 존재) 카운터 샘플이 2개 미만인 신규
// pod 의 짧은 창 (스크레이프 1~2주기) 에서는 여기서만 일시적으로 사유가 붙을 수 있고 1분 내 자연
// 수렴한다.
type NodePodUsage struct {
	Namespace             string   `json:"namespace"`
	Pod                   string   `json:"pod"`
	UID                   string   `json:"uid,omitempty"`
	Phase                 string   `json:"phase,omitempty"`
	Severity              string   `json:"severity,omitempty"`
	UnobservedReason      string   `json:"unobserved_reason,omitempty"`
	CPUPercent            *float64 `json:"cpu_percent,omitempty"`
	CPUUsageCores         *float64 `json:"cpu_usage_cores,omitempty"`
	MemoryPercent         *float64 `json:"memory_percent,omitempty"`
	MemoryWorkingSetBytes *float64 `json:"memory_working_set_bytes,omitempty"`
	NetworkBytesPerSec    *float64 `json:"network_bytes_per_sec,omitempty"`
}

// GetNodePods godoc
// @Summary      노드 하위 pod별 자원 사용량 목록
// @Description  노드에 스케줄된 pod 별로 신원 (namespace 와 pod 와 uid) 과 CPU (limit 대비 percent 와 절대량 cores), memory (limit 대비 percent 와 working set bytes), network bytes rate (netobs pod bytes 의 5분 rate 합산) 와 상태를 한 응답으로 돌려준다. percent 는 limit 분모라 limit 없는 pod 는 생략되고 절대량 필드로 표현된다 (#328 과 동일 규약). severity 는 pod pressure score 3종 최대의 환산 (low/elevated/high) 이고, 종료 pod 와 미관측 사유 (unobserved_reason, no_traffic 포함 #342) 는 pods API 의 규약 (#314, #320) 을 그대로 쓴다. 관측 판정 소스는 rate 결과 존재라 (pods API 는 시리즈 존재) 카운터 샘플이 부족한 신규 pod 의 짧은 창에서는 여기서만 일시적으로 사유가 붙을 수 있고 1분 내 자연 수렴한다. 정렬은 namespace 와 pod 사전순으로 결정적이며 이상 우선 정렬은 severity 를 근거로 표시 계층이 수행한다. at 파라미터로 사건 시점을 재구성할 수 있다.
// @Tags         interference
// @Produce      json
// @Param        node  path   string  true   "노드 이름 (DNS-1123 형식)"
// @Param        at    query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  NodePodsResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/node/{node}/pods [get]
func (h *SynthesisHandler) GetNodePods(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/node/")
	raw := strings.TrimSuffix(strings.Trim(rest, "/"), "/pods")
	node, err := parseNodeParam(raw)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	if node == "" {
		apicommon.WriteError(w, http.StatusBadRequest, "missing_node", "경로는 /api/v1/node/{node}/pods 형식이어야 합니다")
		return
	}
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := NodePodsResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Node:        node,
		Pods:        []NodePodUsage{},
	}
	if h.querier == nil {
		resp.Summary = buildNodePodsSummary(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// node 는 parseNodeParam 검증을 통과한 값이라 %q 결합이 안전하다. cadvisor 합산은 pod-level
	// cgroup 행 (container="", pod!="") 규약 (#313, #336) 을 재사용하고, limits 는 kube-state
	// 롤링 중 동일 시리즈 중복이 sum 을 2배로 만들 수 있어 container 단위 max 로 dedup 후
	// 합산한다 (node-vitals 의 allocatable max 집계와 동일 축). phase 는 node 라벨이 없어 전체
	// 조회 후 uid 로 join 한다 (node-map 규약).
	sel := fmt.Sprintf("{node=%q}", node)
	podLevelSel := promSelector(nodeMatcher(node), `container=""`, `pod!=""`)
	res := h.queryParallel(ctx,
		"kube_pod_info"+sel,
		"kube_pod_status_phase",
		fmt.Sprintf("sum by(namespace, pod) (rate(container_cpu_usage_seconds_total%s[5m]))", podLevelSel),
		"sum by(namespace, pod) (container_memory_working_set_bytes"+podLevelSel+")",
		fmt.Sprintf("sum by(namespace, pod) (max by(namespace, pod, container) (kube_pod_container_resource_limits{node=%q, resource=\"cpu\"}))", node),
		fmt.Sprintf("sum by(namespace, pod) (max by(namespace, pod, container) (kube_pod_container_resource_limits{node=%q, resource=\"memory\"}))", node),
		fmt.Sprintf("sum by(src_namespace, src_pod) (rate(netobs_pod_bytes_total%s[5m]))", sel),
		"pod:cpu_throttle_score:5m"+sel,
		"pod:memory_pressure_score:5m"+sel,
		"pod:network_pressure_score:5m"+sel,
		// #320 미관측 사유 판별용 agent 배치 여부. 단일 노드라 이 노드의 시리즈 존재만 본다.
		"netobs_bpf_program_loaded"+sel,
		// #342 무소켓 pod 집합 (no_traffic 판별 입력).
		fmt.Sprintf("count by(src_namespace, src_pod) (netobs_pod_no_sockets%s)", sel),
	)

	phase := map[string]string{}
	for _, sm := range res[1] {
		if sm.Value == 1 {
			phase[sm.Labels["uid"]] = sm.Labels["phase"]
		}
	}
	type nsPod struct{ ns, pod string }
	byNsPod := func(samples []correlation.InstantSample) map[nsPod]float64 {
		out := map[nsPod]float64{}
		for _, sm := range samples {
			if math.IsNaN(sm.Value) {
				continue
			}
			out[nsPod{sm.Labels["namespace"], sm.Labels["pod"]}] = sm.Value
		}
		return out
	}
	cores := byNsPod(res[2])
	workingSet := byNsPod(res[3])
	cpuLimit := byNsPod(res[4])
	memLimit := byNsPod(res[5])
	network := map[nsPod]float64{}
	observed := map[nsPod]bool{}
	for _, sm := range res[6] {
		if math.IsNaN(sm.Value) {
			continue
		}
		k := nsPod{sm.Labels["src_namespace"], sm.Labels["src_pod"]}
		network[k] = sm.Value
		observed[k] = true
	}
	// severity 입력: pressure 3종의 pod 별 최대.
	maxScore := map[nsPod]float64{}
	for _, samples := range [][]correlation.InstantSample{res[7], res[8], res[9]} {
		for _, sm := range samples {
			if math.IsNaN(sm.Value) {
				continue
			}
			k := nsPod{sm.Labels["src_namespace"], sm.Labels["src_pod"]}
			if v, ok := maxScore[k]; !ok || sm.Value > v {
				maxScore[k] = sm.Value
			}
		}
	}
	agentNodes := map[string]bool{node: len(res[10]) > 0}
	noSockets := map[nsPod]bool{}
	for _, sm := range res[11] {
		noSockets[nsPod{sm.Labels["src_namespace"], sm.Labels["src_pod"]}] = true
	}

	for _, sm := range res[0] {
		ns, pod := sm.Labels["namespace"], sm.Labels["pod"]
		if pod == "" {
			continue
		}
		k := nsPod{ns, pod}
		p := NodePodUsage{
			Namespace: ns,
			Pod:       pod,
			UID:       sm.Labels["uid"],
			Phase:     phase[sm.Labels["uid"]],
		}
		if v, ok := cores[k]; ok {
			p.CPUUsageCores = &v
			if lim, ok := cpuLimit[k]; ok && lim > 0 {
				pct := v / lim * 100
				p.CPUPercent = &pct
			}
		}
		if v, ok := workingSet[k]; ok {
			p.MemoryWorkingSetBytes = &v
			if lim, ok := memLimit[k]; ok && lim > 0 {
				pct := v / lim * 100
				p.MemoryPercent = &pct
			}
		}
		if v, ok := network[k]; ok {
			p.NetworkBytesPerSec = &v
		}
		if v, ok := maxScore[k]; ok {
			p.Severity = correlation.PressureSeverity(v)
		}
		// 미관측 사유. 종료 / Unknown phase 는 사유 판정이 무의미해 생략한다 (pods API 공용 조건).
		if !observed[k] && !unobservedReasonExempt(p.Phase) {
			p.UnobservedReason = unobservedReason(node, sm.Labels["pod_ip"], sm.Labels["host_ip"], agentNodes, noSockets[k])
		}
		resp.Pods = append(resp.Pods, p)
	}

	sort.Slice(resp.Pods, func(i, j int) bool {
		if resp.Pods[i].Namespace != resp.Pods[j].Namespace {
			return resp.Pods[i].Namespace < resp.Pods[j].Namespace
		}
		return resp.Pods[i].Pod < resp.Pods[j].Pod
	})

	resp.Summary = buildNodePodsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildNodePodsSummary 는 노드 pod 수와 elevated 이상 pod 수, 미관측 pod 수를 한 줄로 적는다.
func buildNodePodsSummary(r NodePodsResponse) string {
	elevated, unobserved := 0, 0
	for _, p := range r.Pods {
		if p.Severity == "elevated" || p.Severity == "high" {
			elevated++
		}
		if p.UnobservedReason != "" {
			unobserved++
		}
	}
	return fmt.Sprintf("%s pod %d (elevated 이상 %d, 미관측 %d)", r.Node, len(r.Pods), elevated, unobserved)
}
