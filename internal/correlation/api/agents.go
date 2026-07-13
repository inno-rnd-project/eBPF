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

// agents 는 #266 의 노드별 관측 에이전트 self-health API 다. netobs 와 gpuobs 에이전트의 스크레이프
// up 여부, BPF attach 상태, NVML 오류율, informer lag 를 노드 단위로 집계하고, 기존 PrometheusRule
// 알림 규칙과 동일한 임계로 healthy / degraded 를 판정한다. issues 의 alertname 은 playbooks?cause=
// 조회 입력과 호환된다. ServiceMonitor relabeling 이 모든 에이전트 메트릭에 node 라벨을 부착하므로
// instance 매핑 없이 by(node) 로 직접 집계한다.

// AgentsResponse 는 GET /api/v1/agents 의 typed 응답이다.
type AgentsResponse struct {
	GeneratedAt string        `json:"generated_at"`
	Agents      []AgentHealth `json:"agents"`
	Summary     string        `json:"summary"`
}

// AgentHealth 는 한 노드의 한 에이전트 (netobs 또는 gpuobs) 건강이다. 에이전트 종류에 없는 신호
// (netobs 의 NVML, gpuobs 의 BPF) 는 pointer 생략된다.
type AgentHealth struct {
	Node  string `json:"node"`
	Agent string `json:"agent"`
	Up    bool   `json:"up"`
	// BpfProgramsLoaded / Total 은 netobs 전용. loaded < total 이면 일부 kprobe 가 detach 상태다.
	BpfProgramsLoaded *float64 `json:"bpf_programs_loaded,omitempty"`
	BpfProgramsTotal  *float64 `json:"bpf_programs_total,omitempty"`
	// BpfAttachFailures5m 는 최근 5분 attach 실패 누적 (netobs 전용) 이다.
	BpfAttachFailures5m *float64 `json:"bpf_attach_failures_5m,omitempty"`
	// NvmlErrorsPerSec 는 NVML 호출 실패율 (gpuobs 전용) 이다.
	NvmlErrorsPerSec *float64 `json:"nvml_errors_per_sec,omitempty"`
	// InformerLagSeconds 는 kube informer 마지막 watch event 이후 경과 초다.
	InformerLagSeconds *float64 `json:"informer_lag_seconds,omitempty"`
	// Status 는 healthy 또는 degraded 다. 판정 임계는 기존 알림 규칙과 동일하고, 걸린 규칙의
	// alertname 이 Issues 에 담긴다.
	Status string   `json:"status"`
	Issues []string `json:"issues,omitempty"`
}

// 판정 임계는 deploy/gpuobs/base/prometheus-rule.yaml 의 알림 규칙과 동일 값이다. 규칙이 바뀌면
// 함께 갱신한다.
const (
	agentInformerStaleSeconds = 300 // ObsAgentInformerStale: lag > 300s
	agentNvmlErrorRatePerSec  = 1   // GPUObsAgentNvmlErrorsHigh: rate > 1/s
)

// GetAgents godoc
// @Summary      노드별 관측 에이전트 self-health
// @Description  netobs 와 gpuobs 에이전트의 스크레이프 up 여부, BPF program attach 상태와 최근 attach 실패, NVML 오류율, informer lag 를 노드 단위로 집계하고 기존 알림 규칙과 동일한 임계로 healthy / degraded 를 판정한다. issues 의 alertname 은 playbooks 조회 입력과 호환된다. node 파라미터로 단일 노드를 조회한다.
// @Tags         meta
// @Produce      json
// @Param        node  query  string  false  "단일 노드 필터 (DNS-1123 형식, 생략 시 전체)"
// @Param        at    query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  AgentsResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/agents [get]
func (h *SynthesisHandler) GetAgents(w http.ResponseWriter, r *http.Request) {
	node, err := parseNodeParam(strings.TrimSpace(r.URL.Query().Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := AgentsResponse{GeneratedAt: evalAt.Format(time.RFC3339), Agents: []AgentHealth{}}
	if h.querier == nil {
		resp.Summary = buildAgentsSummary(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// job 매처는 고정 리터럴이고 node 는 parseNodeParam 검증을 통과한 값이라 결합이 안전하다.
	sel := promSelector(nodeMatcher(node))
	upSel := promSelector(`job=~"netobs-agent|gpuobs-agent"`, nodeMatcher(node))

	res := h.queryParallel(ctx,
		"up"+upSel,
		"sum by(node) (netobs_bpf_program_loaded"+sel+")",
		"count by(node) (netobs_bpf_program_loaded"+sel+")",
		fmt.Sprintf(`sum by(node) (increase(netobs_bpf_program_attach_total{result="failure"%s}[5m]))`, nodeSuffixMatcher(node)),
		"sum by(node) (rate(gpuobs_nvml_errors_total"+sel+"[5m]))",
		"max by(node) (netobs_informer_sync_lag_seconds"+sel+")",
		"max by(node) (gpuobs_informer_sync_lag_seconds"+sel+")",
	)

	// (node, agent) 항목의 골격은 up 시리즈에서 만든다. job 라벨이 agent 종류다.
	type agentKey struct{ node, agent string }
	agents := map[agentKey]*AgentHealth{}
	order := []agentKey{}
	for _, sm := range res[0] {
		n, job := sm.Labels["node"], sm.Labels["job"]
		if n == "" || job == "" {
			continue
		}
		agent := strings.TrimSuffix(job, "-agent")
		k := agentKey{n, agent}
		if _, ok := agents[k]; ok {
			continue
		}
		agents[k] = &AgentHealth{Node: n, Agent: agent, Up: sm.Value == 1}
		order = append(order, k)
	}

	byNode := func(samples []correlation.InstantSample, agent string, set func(a *AgentHealth, v float64)) {
		for _, sm := range samples {
			if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
				continue
			}
			if a, ok := agents[agentKey{sm.Labels["node"], agent}]; ok {
				set(a, sm.Value)
			}
		}
	}
	byNode(res[1], "netobs", func(a *AgentHealth, v float64) { a.BpfProgramsLoaded = &v })
	byNode(res[2], "netobs", func(a *AgentHealth, v float64) { a.BpfProgramsTotal = &v })
	byNode(res[3], "netobs", func(a *AgentHealth, v float64) { a.BpfAttachFailures5m = &v })
	byNode(res[4], "gpuobs", func(a *AgentHealth, v float64) { a.NvmlErrorsPerSec = &v })
	byNode(res[5], "netobs", func(a *AgentHealth, v float64) { a.InformerLagSeconds = &v })
	byNode(res[6], "gpuobs", func(a *AgentHealth, v float64) { a.InformerLagSeconds = &v })

	for _, k := range order {
		a := agents[k]
		a.Status, a.Issues = judgeAgentHealth(a)
		resp.Agents = append(resp.Agents, *a)
	}
	sort.Slice(resp.Agents, func(i, j int) bool {
		if resp.Agents[i].Node != resp.Agents[j].Node {
			return resp.Agents[i].Node < resp.Agents[j].Node
		}
		return resp.Agents[i].Agent < resp.Agents[j].Agent
	})

	resp.Summary = buildAgentsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// nodeSuffixMatcher 는 기존 라벨 매처 뒤에 이어 붙일 ", node=..." 조각을 만든다. node 가 비면 빈
// 문자열이라 기존 selector 가 그대로 유지된다.
func nodeSuffixMatcher(node string) string {
	if node == "" {
		return ""
	}
	return fmt.Sprintf(", node=%q", node)
}

// judgeAgentHealth 는 알림 규칙과 동일한 임계로 healthy / degraded 를 판정하고 걸린 규칙의
// alertname 을 issues 로 돌려준다.
func judgeAgentHealth(a *AgentHealth) (string, []string) {
	issues := []string{}
	if !a.Up {
		issues = append(issues, "ObsAgentDown")
	}
	if a.BpfProgramsLoaded != nil && a.BpfProgramsTotal != nil && *a.BpfProgramsLoaded < *a.BpfProgramsTotal {
		issues = append(issues, "NetObsBpfProgramUnavailable")
	}
	if a.BpfAttachFailures5m != nil && *a.BpfAttachFailures5m > 0 {
		issues = append(issues, "NetObsBpfAttachFailureHigh")
	}
	if a.NvmlErrorsPerSec != nil && *a.NvmlErrorsPerSec > agentNvmlErrorRatePerSec {
		issues = append(issues, "GPUObsAgentNvmlErrorsHigh")
	}
	if a.InformerLagSeconds != nil && *a.InformerLagSeconds > agentInformerStaleSeconds {
		issues = append(issues, "ObsAgentInformerStale")
	}
	if len(issues) > 0 {
		return "degraded", issues
	}
	return "healthy", nil
}

// buildAgentsSummary 는 에이전트 수와 degraded 수를 한 줄로 적는다.
func buildAgentsSummary(r AgentsResponse) string {
	degraded := 0
	for _, a := range r.Agents {
		if a.Status != "healthy" {
			degraded++
		}
	}
	return fmt.Sprintf("에이전트 %d개 (degraded %d)", len(r.Agents), degraded)
}
