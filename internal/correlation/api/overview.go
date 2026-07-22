package api

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// overview 는 #249 의 랜딩 대시보드 요약 카드 API 다. 노드 3단 상태와 pod 관측 커버리지 (구조적 미관측 unobservable 분리, #320), firing
// alert severity 집계, GPU fleet, weakest signal 을 한 응답으로 합성해 랜딩 진입 시 프론트가 5~6개
// 엔드포인트를 join 하던 것을 대체한다. 판정은 전부 기존 규약 (alert 라벨 매칭 #248, 압박 임계,
// synthDimensions) 의 재사용이다.

// OverviewResponse 는 GET /api/v1/overview 의 typed 응답이다.
type OverviewResponse struct {
	GeneratedAt string         `json:"generated_at"`
	Nodes       OverviewNodes  `json:"nodes"`
	Pods        OverviewPods   `json:"pods"`
	Issues      OverviewIssues `json:"issues"`
	GPU         OverviewGPU    `json:"gpu"`
	// Weakest 는 health 의 가장 약한 고리 (#248) 와 동일 판정이다. health 를 아는 차원이 없으면
	// 생략된다.
	Weakest *WeakestSignal `json:"weakest,omitempty"`
	Summary string         `json:"summary"`
}

// OverviewNodes 는 노드 3단 상태 집계다. down 은 Ready false, warning 은 Ready 이면서 해당 노드를
// 가리키는 firing alert 가 있거나 통합 압박 (node:pressure_score:5m) 이 elevated 임계를 넘는 노드다.
type OverviewNodes struct {
	Total   int `json:"total"`
	Healthy int `json:"healthy"`
	Warning int `json:"warning"`
	Down    int `json:"down"`
}

// OverviewPods 는 pod 관측 커버리지 집계다. live 는 netobs eBPF 시리즈가 존재하는 pod (pods API 의
// observed 와 동일 판정) 이고, terminated 는 종료 pod (phase Succeeded/Failed, #314) 로 telemetry
// 부재가 정상이라 no_data 분모에서 제외된다. unobservable 은 구조적 미관측 (#320, agent 미배치
// 또는 hostNetwork 로 IP 귀속 불가) 으로 관측 결함이 아니라 역시 분모에서 제외되며, no_data 는
// 실행 중이고 관측 가능한 pod 기준의 나머지다. node-map 의 completed 상태는 Succeeded 만
// 가리키므로 (Failed 는 down), 상위 집합인 본 집계는 terminated 로 명명해 의미 충돌을 피한다.
type OverviewPods struct {
	Total        int `json:"total"`
	Live         int `json:"live"`
	NoData       int `json:"no_data"`
	Unobservable int `json:"unobservable"`
	Terminated   int `json:"terminated"`
}

// OverviewIssues 는 firing alert 의 alertname 단위 집계다. 동일 alert 가 라벨만 다르게 다건 발화해도
// 1건으로 세어 다중 시리즈 인플레를 막는다. critical / warning 외 severity 는 total 에만 계상된다.
type OverviewIssues struct {
	Total    int `json:"total"`
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
}

// OverviewGPU 는 GPU fleet 규모다. kube_node_status_capacity 의 nvidia_com_gpu 선언 기준이라
// gpuobs 수집 여부와 무관하게 안정적이다.
type OverviewGPU struct {
	Nodes   int `json:"nodes"`
	Devices int `json:"devices"`
}

// nodeStatus 는 노드 3단 판정이다 (#249). overview 의 집계와 node-map 의 노드별 상태가 공유한다.
func nodeStatus(ready bool, hasFiringAlert bool, pressure float64) string {
	if !ready {
		return "down"
	}
	if hasFiringAlert || pressure > correlation.PressureElevatedThreshold {
		return "warning"
	}
	return "healthy"
}

// GetOverview godoc
// @Summary      랜딩 대시보드 요약
// @Description  랜딩 요약 카드 5장 (노드 3단 상태, pod 관측 커버리지, firing alert severity 집계, GPU fleet, weakest signal) 을 한 응답으로 돌려준다. 노드 warning 은 firing alert 또는 통합 압박 elevated 초과, pod live 는 netobs eBPF 시리즈 존재, issues 는 alertname 단위 집계다. at 파라미터로 사건 시점의 랜딩 화면을 재구성할 수 있다.
// @Tags         meta
// @Produce      json
// @Param        at  query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  OverviewResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/overview [get]
func (h *SynthesisHandler) GetOverview(w http.ResponseWriter, r *http.Request) {
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}
	resp := OverviewResponse{GeneratedAt: evalAt.Format(time.RFC3339)}
	if h.querier == nil {
		resp.Summary = buildOverviewSummary(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	queries := []string{
		"kube_node_info",
		`kube_node_status_condition{condition="Ready",status="true"}`,
		`ALERTS{alertstate="firing"}`,
		"node:pressure_score:5m",
		"kube_pod_info",
		"count by(src_namespace, src_pod) (netobs_pod_bytes_total)",
		`kube_node_status_capacity{resource="nvidia_com_gpu"}`,
	}
	// weakest 판정용 차원 health 4종은 synthDimensions 선언 순서 (사전순) 로 뒤에 붙인다.
	for _, d := range synthDimensions {
		queries = append(queries, d.healthMetric)
	}
	// #314 종료 pod 구분용 phase. 기존 인덱스를 흔들지 않게 맨 뒤에 붙이고 == 1 로 활성 phase 만 받는다.
	phaseIdx := len(queries)
	queries = append(queries, "kube_pod_status_phase == 1")
	// #320 미관측 사유 판별용 netobs agent 배치 노드 집합.
	agentIdx := len(queries)
	queries = append(queries, "count by(node) (netobs_bpf_program_loaded)")
	res := h.queryParallel(evalCtx, queries...)

	// 노드 3단 상태. ready / firing alert / 압박을 노드별로 모은 뒤 nodeStatus 로 판정한다.
	ready := map[string]bool{}
	for _, sm := range res[1] {
		if name := sm.Labels["node"]; name != "" && sm.Value == 1 {
			ready[name] = true
		}
	}
	alertedNodes := map[string]bool{}
	for _, sm := range res[2] {
		if node := sm.Labels["node"]; node != "" {
			alertedNodes[node] = true
		}
	}
	pressure := map[string]float64{}
	for _, sm := range res[3] {
		if name := sm.Labels["node"]; name != "" && !math.IsNaN(sm.Value) {
			pressure[name] = sm.Value
		}
	}
	// kube-state-metrics 롤링 업데이트 중에는 동일 노드 시계열이 instance 라벨만 다르게 중복될 수
	// 있어 이름 기준으로 dedup 한다. nodes / node-map 은 map 병합이라 자연 면역이고 본 핸들러만
	// 카운터 직접 증가라 가드가 필요하다.
	seenNodes := map[string]bool{}
	for _, sm := range res[0] {
		name := sm.Labels["node"]
		if name == "" || seenNodes[name] {
			continue
		}
		seenNodes[name] = true
		resp.Nodes.Total++
		switch nodeStatus(ready[name], alertedNodes[name], pressure[name]) {
		case "down":
			resp.Nodes.Down++
		case "warning":
			resp.Nodes.Warning++
		default:
			resp.Nodes.Healthy++
		}
	}

	// pod 관측 커버리지. pods API 의 observed 와 동일하게 netobs 시리즈 존재로 판정한다. #314
	// 종료 pod (Succeeded/Failed) 는 telemetry 부재가 정상이라 terminated 로 따로 세고 no-data
	// 분모에서 제외한다.
	observed := map[[2]string]bool{}
	for _, sm := range res[5] {
		observed[[2]string{sm.Labels["src_namespace"], sm.Labels["src_pod"]}] = true
	}
	terminal := map[[2]string]bool{}
	for _, sm := range res[phaseIdx] {
		if ph := sm.Labels["phase"]; ph == "Succeeded" || ph == "Failed" {
			terminal[[2]string{sm.Labels["namespace"], sm.Labels["pod"]}] = true
		}
	}
	agentNodes := map[string]bool{}
	for _, sm := range res[agentIdx] {
		if n := sm.Labels["node"]; n != "" {
			agentNodes[n] = true
		}
	}
	for _, sm := range res[4] {
		if sm.Labels["pod"] == "" {
			continue
		}
		resp.Pods.Total++
		key := [2]string{sm.Labels["namespace"], sm.Labels["pod"]}
		switch {
		case terminal[key]:
			resp.Pods.Terminated++
		case observed[key]:
			resp.Pods.Live++
		case unobservedReason(sm.Labels["node"], sm.Labels["pod_ip"], sm.Labels["host_ip"], agentNodes) != "no_data":
			// #320 구조적 미관측 (agent 미배치 / hostNetwork) 은 관측 결함이 아니라 분리한다.
			resp.Pods.Unobservable++
		}
	}
	resp.Pods.NoData = resp.Pods.Total - resp.Pods.Terminated - resp.Pods.Live - resp.Pods.Unobservable

	// issues: firing alert 의 alertname 단위 dedup 집계. severity 는 alert 규칙 정의라 동일
	// alertname 내에서 균일하다.
	issueSeverity := map[string]string{}
	for _, sm := range res[2] {
		if name := sm.Labels["alertname"]; name != "" {
			issueSeverity[name] = sm.Labels["severity"]
		}
	}
	for _, sev := range issueSeverity {
		resp.Issues.Total++
		switch sev {
		case "critical":
			resp.Issues.Critical++
		case "warning":
			resp.Issues.Warning++
		}
	}

	// GPU fleet: capacity 선언 기준. devices 는 노드별 선언 수량의 합이며, 노드 집계와 동일하게
	// 중복 시계열을 이름 기준으로 dedup 한다.
	seenGPUNodes := map[string]bool{}
	for _, sm := range res[6] {
		node := sm.Labels["node"]
		if node == "" || seenGPUNodes[node] || math.IsNaN(sm.Value) || sm.Value <= 0 {
			continue
		}
		seenGPUNodes[node] = true
		resp.GPU.Nodes++
		resp.GPU.Devices += int(sm.Value)
	}

	// weakest: 차원 health 최솟값 (#248 health 와 동일 판정, 사전순 동률 결정성).
	for i, d := range synthDimensions {
		s := res[7+i]
		if len(s) == 0 || math.IsNaN(s[0].Value) {
			continue
		}
		v := s[0].Value
		if resp.Weakest == nil || v < resp.Weakest.Health {
			resp.Weakest = &WeakestSignal{Dimension: d.name, Health: v, Status: correlation.HealthStatus(v)}
		}
	}

	resp.Summary = buildOverviewSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildOverviewSummary 는 카드 5장을 한 줄로 요약한다.
func buildOverviewSummary(r OverviewResponse) string {
	weak := "없음"
	if r.Weakest != nil {
		weak = fmt.Sprintf("%s (health %.2f)", r.Weakest.Dimension, r.Weakest.Health)
	}
	return fmt.Sprintf("노드 %d (정상 %d, 경고 %d, down %d), pod %d (관측 %d, no-data %d, 구조 미관측 %d, 종료 %d), 이슈 %d (critical %d), 가장 약한 신호 %s",
		r.Nodes.Total, r.Nodes.Healthy, r.Nodes.Warning, r.Nodes.Down,
		r.Pods.Total, r.Pods.Live, r.Pods.NoData, r.Pods.Unobservable, r.Pods.Terminated, r.Issues.Total, r.Issues.Critical, weak)
}
