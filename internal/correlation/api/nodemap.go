package api

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// node-map 은 #249 의 랜딩 노드 그리드 API 다. 노드별 pod 목록에 3단 상태 (live / warning / down)
// 와 관측 no-data, 해당 pod 를 가리키는 firing alertname 을 서버에서 판정해 내장한다. alertname 은
// rca?alert= 와 playbooks?cause= 링크 입력과 호환되어 그리드에서 사건 여정으로 바로 진입한다.

// NodeMapResponse 는 GET /api/v1/node-map 의 typed 응답이다. 노드는 pod 수 내림차순 (동률 이름
// 사전순) 으로 정렬되어 그리드 배치 순서와 일치한다.
type NodeMapResponse struct {
	GeneratedAt string        `json:"generated_at"`
	Nodes       []NodeMapNode `json:"nodes"`
	Summary     string        `json:"summary"`
}

// NodeMapNode 는 그리드의 한 노드 칸이다. Status 판정은 overview 와 동일 (nodeStatus 공유) 하다.
type NodeMapNode struct {
	Name       string       `json:"name"`
	Roles      []string     `json:"roles,omitempty"`
	GPU        bool         `json:"gpu"`
	GPUDevices int          `json:"gpu_devices,omitempty"`
	Status     string       `json:"status"`
	PodCount   int          `json:"pod_count"`
	Pods       []NodeMapPod `json:"pods"`
}

// NodeMapPod 는 노드 칸 안의 한 pod 뱃지다. down 은 phase Failed/Unknown, warning 은 Pending 또는
// 이 pod 를 가리키는 firing alert 존재, completed 는 phase Succeeded (정상 종료라 telemetry 부재가
// 정상, #314), live 는 Running 이다. NoData 는 pods API 의 observed
// 반전과 동일 판정이다.
type NodeMapPod struct {
	Namespace string   `json:"namespace"`
	Pod       string   `json:"pod"`
	Status    string   `json:"status"`
	NoData    bool     `json:"no_data,omitempty"`
	// NoDataReason 은 NoData 일 때의 미관측 사유 분류다 (#320, pods API 의 unobserved_reason 과
	// 동일 enum). 종료 pod 는 사유가 생략된다.
	NoDataReason string `json:"no_data_reason,omitempty"`
	Issues    []string `json:"issues,omitempty"`
}

// GetNodeMap godoc
// @Summary      노드 그리드 (노드별 pod 상태 맵)
// @Description  노드별 pod 목록에 4단 상태 (live/warning/down/completed) 와 관측 no-data, 해당 pod 를 가리키는 firing alertname 을 내장해 돌려준다. pod down 은 phase Failed/Unknown, warning 은 Pending 또는 firing alert 매칭, completed 는 phase Succeeded (정상 종료라 telemetry 부재가 정상, #314) 이며, issues 의 alertname 은 rca 와 playbooks 조회 입력과 호환된다. no-data 뱃지에는 미관측 사유 (no_data_reason: agent_absent 는 노드에 netobs 미배치, host_network 는 IP 귀속 불가, no_data 는 시리즈 부재, #320) 가 붙는다. node 파라미터로 단일 노드를 조회하고 미등록 노드는 404 다. at 파라미터로 사건 시점의 그리드를 재구성할 수 있다.
// @Tags         inventory
// @Produce      json
// @Param        node  query  string  false  "단일 노드 조회 (미등록 노드는 404)"
// @Param        at    query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  NodeMapResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      404  {object}  apicommon.ErrorBody
// @Router       /api/v1/node-map [get]
func (h *SynthesisHandler) GetNodeMap(w http.ResponseWriter, r *http.Request) {
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}
	nodeFilter := strings.TrimSpace(r.URL.Query().Get("node"))
	resp := NodeMapResponse{GeneratedAt: evalAt.Format(time.RFC3339), Nodes: []NodeMapNode{}}
	if h.querier == nil {
		resp.Summary = buildNodeMapSummary(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	res := h.queryParallel(evalCtx,
		"kube_node_info",
		`kube_node_status_condition{condition="Ready",status="true"}`,
		"kube_node_role",
		`kube_node_status_capacity{resource="nvidia_com_gpu"}`,
		"node:pressure_score:5m",
		`ALERTS{alertstate="firing"}`,
		"kube_pod_info",
		"kube_pod_status_phase",
		"count by(src_namespace, src_pod) (netobs_pod_bytes_total)",
		// #320 미관측 사유 판별용 netobs agent 배치 노드 집합.
		"count by(node) (netobs_bpf_program_loaded)",
	)

	// 노드 골격. overview 와 동일 소스에서 ready / roles / GPU capacity / 압박을 모은다.
	nodes := map[string]*NodeMapNode{}
	for _, sm := range res[0] {
		if name := sm.Labels["node"]; name != "" {
			nodes[name] = &NodeMapNode{Name: name, Pods: []NodeMapPod{}}
		}
	}
	ready := map[string]bool{}
	for _, sm := range res[1] {
		if name := sm.Labels["node"]; name != "" && sm.Value == 1 {
			ready[name] = true
		}
	}
	for _, sm := range res[2] {
		if n, ok := nodes[sm.Labels["node"]]; ok && sm.Labels["role"] != "" {
			n.Roles = append(n.Roles, sm.Labels["role"])
		}
	}
	for _, sm := range res[3] {
		if n, ok := nodes[sm.Labels["node"]]; ok && !math.IsNaN(sm.Value) && sm.Value > 0 {
			n.GPU = true
			n.GPUDevices = int(sm.Value)
		}
	}
	pressure := map[string]float64{}
	for _, sm := range res[4] {
		if name := sm.Labels["node"]; name != "" && !math.IsNaN(sm.Value) {
			pressure[name] = sm.Value
		}
	}
	alertedNodes := map[string]bool{}
	firing := res[5]
	for _, sm := range firing {
		if node := sm.Labels["node"]; node != "" {
			alertedNodes[node] = true
		}
	}
	for name, n := range nodes {
		sort.Strings(n.Roles)
		n.Status = nodeStatus(ready[name], alertedNodes[name], pressure[name])
	}

	// pod 골격과 상태. phase 는 uid join, 관측 커버리지와 alert 매칭은 (namespace, pod) 기준이다.
	phase := map[string]string{}
	for _, sm := range res[7] {
		if sm.Value == 1 {
			phase[sm.Labels["uid"]] = sm.Labels["phase"]
		}
	}
	agentNodes := map[string]bool{}
	for _, sm := range res[9] {
		if nn := sm.Labels["node"]; nn != "" {
			agentNodes[nn] = true
		}
	}
	observed := map[[2]string]bool{}
	for _, sm := range res[8] {
		observed[[2]string{sm.Labels["src_namespace"], sm.Labels["src_pod"]}] = true
	}
	for _, sm := range res[6] {
		ns, pod, node := sm.Labels["namespace"], sm.Labels["pod"], sm.Labels["node"]
		// 단일 노드 조회면 타 노드 pod 의 판정 (특히 firing 순회하는 podIssues) 을 건너뛴다.
		if nodeFilter != "" && node != nodeFilter {
			continue
		}
		n, ok := nodes[node]
		if pod == "" || !ok {
			continue
		}
		p := NodeMapPod{
			Namespace: ns,
			Pod:       pod,
			NoData:    !observed[[2]string{ns, pod}],
			Issues:    podIssues(firing, ns, pod),
		}
		p.Status = podStatus(phase[sm.Labels["uid"]], len(p.Issues) > 0)
		// #320 미관측 사유. 종료 pod (completed/down) 는 telemetry 부재가 정상이라 생략한다.
		if p.NoData && !podTerminated(phase[sm.Labels["uid"]]) && phase[sm.Labels["uid"]] != "Unknown" {
			p.NoDataReason = unobservedReason(node, sm.Labels["pod_ip"], sm.Labels["host_ip"], agentNodes)
		}
		n.Pods = append(n.Pods, p)
	}

	for _, n := range nodes {
		n.PodCount = len(n.Pods)
		sort.Slice(n.Pods, func(i, j int) bool {
			if n.Pods[i].Namespace != n.Pods[j].Namespace {
				return n.Pods[i].Namespace < n.Pods[j].Namespace
			}
			return n.Pods[i].Pod < n.Pods[j].Pod
		})
	}

	if nodeFilter != "" {
		n, ok := nodes[nodeFilter]
		if !ok {
			apicommon.WriteError(w, http.StatusNotFound, "unknown_node", "미등록 노드: "+nodeFilter)
			return
		}
		resp.Nodes = []NodeMapNode{*n}
	} else {
		for _, n := range nodes {
			resp.Nodes = append(resp.Nodes, *n)
		}
		// 그리드 배치 순서: pod 수 내림차순, 동률은 이름 사전순.
		sort.Slice(resp.Nodes, func(i, j int) bool {
			if resp.Nodes[i].PodCount != resp.Nodes[j].PodCount {
				return resp.Nodes[i].PodCount > resp.Nodes[j].PodCount
			}
			return resp.Nodes[i].Name < resp.Nodes[j].Name
		})
	}

	resp.Summary = buildNodeMapSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// podStatus 는 pod 4단 판정이다 (#314 completed 추가). phase 가 미상 (enrich 실패) 이면 alert
// 만으로 판정한다. Succeeded 는 stale alert 가 남아 있어도 completed 가 우선한다 (정상 종료).
func podStatus(phase string, hasIssue bool) string {
	switch phase {
	case "Failed", "Unknown":
		return "down"
	case "Pending":
		return "warning"
	case "Succeeded":
		return "completed"
	}
	if hasIssue {
		return "warning"
	}
	return "live"
}

// podIssues 는 이 pod 를 가리키는 firing alertname 의 dedup 정렬 목록이다. 매칭은 #248 의
// alertTargetsPod 규약을 공유하며 #252 부터 namespace 를 함께 제약해 동명 pod 오탐을 막는다.
func podIssues(firing []correlation.InstantSample, namespace, pod string) []string {
	seen := map[string]bool{}
	for _, sm := range firing {
		if name := sm.Labels["alertname"]; name != "" && alertTargetsPod(sm.Labels, namespace, pod) {
			seen[name] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// buildNodeMapSummary 는 그리드 규모와 경고 pod 수를 한 줄로 적는다.
func buildNodeMapSummary(r NodeMapResponse) string {
	pods, warned, completed := 0, 0, 0
	for _, n := range r.Nodes {
		pods += n.PodCount
		for _, p := range n.Pods {
			switch p.Status {
			case "warning", "down":
				warned++
			case "completed":
				completed++
			}
		}
	}
	return fmt.Sprintf("노드 %d, pod %d (경고·down %d, 종료 %d)", len(r.Nodes), pods, warned, completed)
}
