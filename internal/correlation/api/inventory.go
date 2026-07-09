package api

import (
	"context"
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
// 순차로 병합한다. 실패한 query 는 nil 슬라이스로 남는다.
func (h *SynthesisHandler) queryParallel(ctx context.Context, queries ...string) [][]correlation.InstantSample {
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
	res := h.queryParallel(ctx,
		"kube_node_info",
		`kube_node_status_addresses{type="ExternalIP"}`,
		`kube_node_status_condition{condition="Ready",status="true"}`,
		"kube_node_status_capacity",
		"kube_node_role",
	)

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
// @Description  파드별 namespace, 이름, uid, pod IP, host IP, node, workload(created_by), priority, phase, qos, 관측 커버리지(observed)를 kube-state-metrics 기반으로 돌려준다. observed 는 netobs 의 eBPF 시리즈가 존재하는지로, false 면 관측 no-data pod 다. ?namespace 로 필터한다. 다른 API의 src_namespace/src_pod/pod_uid와 동일 키로 매핑한다.
// @Tags         inventory
// @Produce      json
// @Param        namespace  query  string  false  "namespace 필터 (생략 시 전체)"
// @Success      200  {object}  PodsResponse
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
	res := h.queryParallel(ctx,
		"kube_pod_info",
		"kube_pod_status_phase",
		"kube_pod_status_qos_class",
		"count by(src_namespace, src_pod) (netobs_pod_bytes_total)",
	)

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
	for _, p := range pods {
		p.Observed = observed[[2]string{p.Namespace, p.Pod}]
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
