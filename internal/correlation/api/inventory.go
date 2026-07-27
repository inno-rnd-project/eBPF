package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// 인벤토리 API 는 다른 진단 API 가 돌려주는 node / src_namespace / src_pod / pod_uid 식별자를 사람이
// 읽는 정보와 매핑하도록 기본 k8s 메타데이터를 노출한다. 신규 k8s 클라이언트 없이 kube-state-metrics
// 가 이미 Prometheus 로 수집하는 kube_node_* / kube_pod_* 메트릭을 기존 InstantQuerier 로 읽는다.

// NodeInventory 는 한 노드의 기본 정보다.
type NodeInventory struct {
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	InternalIP string `json:"internal_ip,omitempty"`
	ExternalIP string `json:"external_ip,omitempty"`
	Ready      bool   `json:"ready"`
	// Roles 는 kube_node_role 기반 노드 역할 (control-plane 등) 이다 (#248). 역할 라벨이 없는
	// worker 노드는 생략된다.
	Roles          []string     `json:"roles,omitempty"`
	KubeletVersion string       `json:"kubelet_version,omitempty"`
	RuntimeVersion string       `json:"runtime_version,omitempty"`
	KernelVersion  string       `json:"kernel_version,omitempty"`
	OSImage        string       `json:"os_image,omitempty"`
	Capacity       NodeCapacity `json:"capacity"`
}

// NodeCapacity 는 노드 capacity 다. 없는 자원 (GPU 미설치 등) 은 null 로 생략된다.
type NodeCapacity struct {
	CPU         *float64 `json:"cpu,omitempty"`
	MemoryBytes *float64 `json:"memory_bytes,omitempty"`
	GPU         *float64 `json:"gpu,omitempty"`
}

// PodInventory 는 한 파드의 기본 정보다. workload 는 created_by (보통 ReplicaSet / DaemonSet) 로,
// 다른 API 의 src_workload 와 매핑하는 키다.
type PodInventory struct {
	Namespace     string `json:"namespace"`
	Pod           string `json:"pod"`
	UID           string `json:"uid,omitempty"`
	PodIP         string `json:"pod_ip,omitempty"`
	HostIP        string `json:"host_ip,omitempty"`
	Node          string `json:"node,omitempty"`
	WorkloadKind  string `json:"workload_kind,omitempty"`
	WorkloadName  string `json:"workload_name,omitempty"`
	PriorityClass string `json:"priority_class,omitempty"`
	Phase         string `json:"phase,omitempty"`
	QOSClass      string `json:"qos_class,omitempty"`
	// Observed 는 eBPF 관측 커버리지다 (#248). netobs 가 상시 수집하는 netobs_pod_bytes_total
	// 시리즈가 이 pod 에 존재하면 true, 없으면 no-data 로 판정한다.
	Observed bool `json:"observed"`
	// UnobservedReason 은 미관측 사유 분류다 (#320, #342). agent_absent (노드에 netobs 미배치),
	// host_network (pod IP 가 host IP 와 같아 IP 귀속이 성립하지 않음), no_traffic (netns 소켓
	// 테이블이 비어 네트워크 미사용이 증명됨, unix socket 전용 pod 등), no_data (관측 가능한데
	// 시리즈 부재) 로 구분한다. host_network 는 확정이 아닌 시점 의존 분류로, cgroup 힌트 (#228)
	// 가 TCP 트래픽에서 학습되면 observed=true (live) 로 전환될 수 있다. observed=true 와
	// 종료/Unknown phase 는 생략된다.
	UnobservedReason string `json:"unobserved_reason,omitempty"`
}

// unobservedReason 은 #320 의 미관측 사유 판정이다. 우선순위는 agent_absent (hostNetwork 여부와
// 무관하게 관측 자체가 불가) → host_network → no_traffic (#342, agent 의 소켓 스캔이 netns 무소켓
// 을 증명해 시리즈 부재가 정상) → no_data 다. agentNodes 는 netobs_bpf_program_loaded 가 존재하는
// 노드 집합이고, noTraffic 은 netobs_pod_no_sockets 시리즈 존재 여부다.
func unobservedReason(node, podIP, hostIP string, agentNodes map[string]bool, noTraffic bool) string {
	switch {
	case node != "" && !agentNodes[node]:
		return "agent_absent"
	case podIP != "" && podIP == hostIP:
		return "host_network"
	case noTraffic:
		return "no_traffic"
	default:
		return "no_data"
	}
}

// podTerminated 는 종료 phase (telemetry 부재가 정상인 상태, #314) 판정이다. overview 의
// terminated 집계와 의미를 공유하므로 Unknown 을 포함하지 않는다.
func podTerminated(phase string) bool {
	return phase == "Succeeded" || phase == "Failed"
}

// unobservedReasonExempt 는 미관측 사유 판정을 생략하는 phase 다. 종료는 telemetry 부재가
// 정상이고 (#314), Unknown 은 노드 유실 등으로 pod 상태 자체가 미상이라 사유 분류가 무의미하다.
// pods API 와 node-map 이 같은 조건을 공유한다 (#320).
func unobservedReasonExempt(phase string) bool {
	return podTerminated(phase) || phase == "Unknown"
}

// NodesResponse 는 GET /api/v1/nodes 의 typed 응답이다.
type NodesResponse struct {
	GeneratedAt string          `json:"generated_at"`
	Nodes       []NodeInventory `json:"nodes"`
}

// PodsResponse 는 GET /api/v1/pods 의 typed 응답이다.
type PodsResponse struct {
	GeneratedAt string         `json:"generated_at"`
	Pods        []PodInventory `json:"pods"`
}

// queryParallel 은 여러 instant query 를 goroutine 으로 동시에 실행해 결과를 입력 순서대로 돌려준다.
// 각 goroutine 이 out 슬라이스의 자기 인덱스에만 써 mutex 없이 race-free 하고, 호출부는 배리어 뒤에서
// 순차로 병합한다. 하나라도 query 가 error 를 내면 (#352) 그 첫 error 를 함께 돌려준다. PromQL 은
// "메트릭 부재" 를 error 가 아닌 빈 결과로 돌려주므로 error != nil 은 항상 Prometheus 백엔드 장애
// (timeout / transport) 를 뜻한다. 따라서 호출부는 error 를 500 query_failed 로 통일해 데이터 부재
// (빈 결과, error nil → 200) 와 백엔드 장애를 상태코드로 구분한다. 보조 신호만 다루는 best-effort
// 호출부 (fetchCauseEvidence) 만 error 를 의도적으로 무시한다.
func (h *SynthesisHandler) queryParallel(ctx context.Context, queries ...string) ([][]correlation.InstantSample, error) {
	out := make([][]correlation.InstantSample, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			s, err := h.querier.Query(ctx, q)
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = s
		}(i, q)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return out, e
		}
	}
	return out, nil
}

// queryParallelOptional 은 best-effort 부가 신호 전용 병렬 조회다 (#352 리뷰). queryParallel 과 달리
// error 를 전파하지 않고 실패한 query 를 빈 슬라이스로 남긴다. 필수 데이터와 부가 신호를 한 handler
// 에서 함께 조회하는 endpoint (overview 의 weakest health 등) 가 부가 신호의 부분 실패로 전체 500 이
// 되지 않도록, 필수는 queryParallel (500 게이트), 부가는 본 함수 (degrade) 로 분리한다.
func (h *SynthesisHandler) queryParallelOptional(ctx context.Context, queries ...string) [][]correlation.InstantSample {
	out := make([][]correlation.InstantSample, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			if s, err := h.querier.Query(ctx, q); err == nil {
				out[i] = s
			}
		}(i, q)
	}
	wg.Wait()
	return out
}

// GetNodes godoc
// @Summary      노드 인벤토리
// @Description  노드별 이름, uid, 내부/외부 IP, Ready 상태, 역할(control-plane 등), 버전, capacity(cpu/memory/gpu)를 kube-state-metrics 기반으로 돌려준다. 다른 API의 node 라벨과 동일 키로 매핑한다.
// @Tags         inventory
// @Produce      json
// @Success      200  {object}  NodesResponse
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/nodes [get]
func (h *SynthesisHandler) GetNodes(w http.ResponseWriter, r *http.Request) {
	resp := NodesResponse{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Nodes: []NodeInventory{}}
	if h.querier == nil {
		apicommon.WriteJSON(w, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 4개 메트릭을 동시에 조회하고 (I/O 병렬), 맵 병합은 배리어 뒤에서 순차로 한다.
	res, qerr := h.queryParallel(ctx,
		"kube_node_info",
		`kube_node_status_addresses{type="ExternalIP"}`,
		`kube_node_status_condition{condition="Ready",status="true"}`,
		"kube_node_status_capacity",
		"kube_node_role",
	)
	if qerr != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
		return
	}

	nodes := map[string]*NodeInventory{}
	get := func(name string) *NodeInventory {
		n, ok := nodes[name]
		if !ok {
			n = &NodeInventory{Name: name}
			nodes[name] = n
		}
		return n
	}

	// kube_node_info: uid(system_uuid) + 내부 IP + 버전. base set.
	for _, sm := range res[0] {
		name := sm.Labels["node"]
		if name == "" {
			continue
		}
		n := get(name)
		n.UID = sm.Labels["system_uuid"]
		n.InternalIP = sm.Labels["internal_ip"]
		n.KubeletVersion = sm.Labels["kubelet_version"]
		n.RuntimeVersion = sm.Labels["container_runtime_version"]
		n.KernelVersion = sm.Labels["kernel_version"]
		n.OSImage = sm.Labels["os_image"]
	}
	// 외부 IP 는 kube_node_status_addresses{type="ExternalIP"} 에만 있다.
	for _, sm := range res[1] {
		if name := sm.Labels["node"]; name != "" {
			get(name).ExternalIP = sm.Labels["address"]
		}
	}
	// Ready 조건 (value 1 = ready).
	for _, sm := range res[2] {
		if name := sm.Labels["node"]; name != "" && sm.Value == 1 {
			get(name).Ready = true
		}
	}
	// capacity: cpu / memory / gpu.
	for _, sm := range res[3] {
		name := sm.Labels["node"]
		if name == "" || math.IsNaN(sm.Value) {
			continue
		}
		v := sm.Value
		switch sm.Labels["resource"] {
		case "cpu":
			get(name).Capacity.CPU = &v
		case "memory":
			get(name).Capacity.MemoryBytes = &v
		case "nvidia_com_gpu":
			get(name).Capacity.GPU = &v
		}
	}

	// 역할 (control-plane 등). 라벨이 없는 worker 는 자연 생략되고, 다중 역할은 사전순으로 결정적이다.
	for _, sm := range res[4] {
		if name, role := sm.Labels["node"], sm.Labels["role"]; name != "" && role != "" {
			n := get(name)
			n.Roles = append(n.Roles, role)
		}
	}
	for _, n := range nodes {
		sort.Strings(n.Roles)
	}

	for _, n := range nodes {
		resp.Nodes = append(resp.Nodes, *n)
	}
	sort.Slice(resp.Nodes, func(i, j int) bool { return resp.Nodes[i].Name < resp.Nodes[j].Name })
	apicommon.WriteJSON(w, resp)
}

// GetPods godoc
// @Summary      파드 인벤토리
// @Description  파드별 namespace, 이름, uid, pod IP, host IP, node, workload(created_by), priority, phase, qos, 관측 커버리지(observed)를 kube-state-metrics 기반으로 돌려준다. observed 는 netobs 의 eBPF 시리즈가 존재하는지로, false 면 unobserved_reason 이 미관측 사유를 분류한다 (#320, #342): agent_absent 는 노드에 netobs 미배치, host_network 는 pod IP 가 host IP 와 같아 IP 귀속이 성립하지 않는 상태로 cgroup 힌트가 학습되면 live 로 전환될 수 있는 시점 의존 분류다. no_traffic 은 agent 의 소켓 스캔이 pod netns 의 무소켓을 증명한 상태 (unix socket 전용 pod 등) 로 시리즈 부재가 정상이고, no_data 는 관측 가능한데 시리즈 부재다. 종료 pod (phase Succeeded/Failed) 는 telemetry 부재가 정상이라 observed=false 에 사유가 생략된다 (#314). ?namespace 로 필터한다. 다른 API의 src_namespace/src_pod/pod_uid와 동일 키로 매핑한다.
// @Tags         inventory
// @Produce      json
// @Param        namespace  query  string  false  "namespace 필터 (생략 시 전체)"
// @Success      200  {object}  PodsResponse
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/pods [get]
func (h *SynthesisHandler) GetPods(w http.ResponseWriter, r *http.Request) {
	nsFilter := strings.TrimSpace(r.URL.Query().Get("namespace"))
	resp := PodsResponse{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Pods: []PodInventory{}}
	if h.querier == nil {
		apicommon.WriteJSON(w, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// base(kube_pod_info)와 enrich(phase/qos/관측 커버리지)를 동시에 조회하고 배리어 뒤에서 join 으로
	// 병합한다. 관측 커버리지는 netobs 가 allow-list 무관 상시 수집하는 netobs_pod_bytes_total 의
	// 시리즈 존재로 판정한다 (#248).
	res, qerr := h.queryParallel(ctx,
		"kube_pod_info",
		"kube_pod_status_phase",
		"kube_pod_status_qos_class",
		"count by(src_namespace, src_pod) (netobs_pod_bytes_total)",
		// #320 미관측 사유 판별용 netobs agent 배치 노드 집합.
		"count by(node) (netobs_bpf_program_loaded)",
		// #342 무소켓 pod 집합 (no_traffic 판별 입력).
		"count by(src_namespace, src_pod) (netobs_pod_no_sockets)",
	)
	if qerr != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
		return
	}

	// kube_pod_info: base set. namespace 필터는 PromQL injection 을 피해 Go 측에서 적용한다.
	pods := []*PodInventory{}
	byUID := map[string]*PodInventory{}
	for _, sm := range res[0] {
		ns := sm.Labels["namespace"]
		pod := sm.Labels["pod"]
		if pod == "" || (nsFilter != "" && ns != nsFilter) {
			continue
		}
		p := &PodInventory{
			Namespace:     ns,
			Pod:           pod,
			UID:           sm.Labels["uid"],
			PodIP:         sm.Labels["pod_ip"],
			HostIP:        sm.Labels["host_ip"],
			Node:          sm.Labels["node"],
			WorkloadKind:  sm.Labels["created_by_kind"],
			WorkloadName:  sm.Labels["created_by_name"],
			PriorityClass: sm.Labels["priority_class"],
		}
		pods = append(pods, p)
		if p.UID != "" {
			byUID[p.UID] = p
		}
	}
	// phase / qos 는 uid 로만 라벨링되므로 base 의 uid 집합만 enrich 한다 (value 1 = 활성).
	for _, sm := range res[1] {
		if sm.Value == 1 {
			if p, ok := byUID[sm.Labels["uid"]]; ok {
				p.Phase = sm.Labels["phase"]
			}
		}
	}
	for _, sm := range res[2] {
		if sm.Value == 1 {
			if p, ok := byUID[sm.Labels["uid"]]; ok {
				p.QOSClass = sm.Labels["qos_class"]
			}
		}
	}
	// 관측 커버리지는 netobs 시리즈의 (src_namespace, src_pod) 를 (namespace, pod) 로 join 한다.
	observed := map[[2]string]bool{}
	for _, sm := range res[3] {
		observed[[2]string{sm.Labels["src_namespace"], sm.Labels["src_pod"]}] = true
	}
	agentNodes := map[string]bool{}
	for _, sm := range res[4] {
		if n := sm.Labels["node"]; n != "" {
			agentNodes[n] = true
		}
	}
	noSockets := map[[2]string]bool{}
	for _, sm := range res[5] {
		noSockets[[2]string{sm.Labels["src_namespace"], sm.Labels["src_pod"]}] = true
	}
	for _, p := range pods {
		p.Observed = observed[[2]string{p.Namespace, p.Pod}]
		// #320 미관측 사유. 관측 성공과 사유 판정이 무의미한 phase (종료 / Unknown) 는 생략한다.
		if !p.Observed && !unobservedReasonExempt(p.Phase) {
			p.UnobservedReason = unobservedReason(p.Node, p.PodIP, p.HostIP, agentNodes, noSockets[[2]string{p.Namespace, p.Pod}])
		}
	}

	for _, p := range pods {
		resp.Pods = append(resp.Pods, *p)
	}
	sort.Slice(resp.Pods, func(i, j int) bool {
		if resp.Pods[i].Namespace != resp.Pods[j].Namespace {
			return resp.Pods[i].Namespace < resp.Pods[j].Namespace
		}
		return resp.Pods[i].Pod < resp.Pods[j].Pod
	})
	apicommon.WriteJSON(w, resp)
}
