package api

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"netobs/internal/apicommon"
)

// 인벤토리 API 는 다른 진단 API 가 돌려주는 node / src_namespace / src_pod / pod_uid 식별자를 사람이
// 읽는 정보와 매핑하도록 기본 k8s 메타데이터를 노출한다. 신규 k8s 클라이언트 없이 kube-state-metrics
// 가 이미 Prometheus 로 수집하는 kube_node_* / kube_pod_* 메트릭을 기존 InstantQuerier 로 읽는다.

// NodeInventory 는 한 노드의 기본 정보다.
type NodeInventory struct {
	Name           string       `json:"name"`
	UID            string       `json:"uid,omitempty"`
	InternalIP     string       `json:"internal_ip,omitempty"`
	ExternalIP     string       `json:"external_ip,omitempty"`
	Ready          bool         `json:"ready"`
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

// GetNodes godoc
// @Summary      노드 인벤토리
// @Description  노드별 이름, uid, 내부/외부 IP, Ready 상태, 버전, capacity(cpu/memory/gpu)를 kube-state-metrics 기반으로 돌려준다. 다른 API의 node 라벨과 동일 키로 매핑한다.
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
	if s, err := h.querier.Query(ctx, "kube_node_info"); err == nil {
		for _, sm := range s {
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
	}
	// 외부 IP 는 kube_node_status_addresses{type="ExternalIP"} 에만 있다.
	if s, err := h.querier.Query(ctx, `kube_node_status_addresses{type="ExternalIP"}`); err == nil {
		for _, sm := range s {
			if name := sm.Labels["node"]; name != "" {
				get(name).ExternalIP = sm.Labels["address"]
			}
		}
	}
	// Ready 조건 (value 1 = ready).
	if s, err := h.querier.Query(ctx, `kube_node_status_condition{condition="Ready",status="true"}`); err == nil {
		for _, sm := range s {
			if name := sm.Labels["node"]; name != "" && sm.Value == 1 {
				get(name).Ready = true
			}
		}
	}
	// capacity: cpu / memory / gpu.
	if s, err := h.querier.Query(ctx, "kube_node_status_capacity"); err == nil {
		for _, sm := range s {
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
	}

	for _, n := range nodes {
		resp.Nodes = append(resp.Nodes, *n)
	}
	sort.Slice(resp.Nodes, func(i, j int) bool { return resp.Nodes[i].Name < resp.Nodes[j].Name })
	apicommon.WriteJSON(w, resp)
}

// GetPods godoc
// @Summary      파드 인벤토리
// @Description  파드별 namespace, 이름, uid, pod IP, host IP, node, workload(created_by), priority, phase, qos를 kube-state-metrics 기반으로 돌려준다. ?namespace 로 필터한다. 다른 API의 src_namespace/src_pod/pod_uid와 동일 키로 매핑한다.
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

	// kube_pod_info: base set. namespace 필터는 PromQL injection 을 피해 Go 측에서 적용한다.
	pods := []*PodInventory{}
	byUID := map[string]*PodInventory{}
	if s, err := h.querier.Query(ctx, "kube_pod_info"); err == nil {
		for _, sm := range s {
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
	}
	// phase / qos 는 uid 로만 라벨링되므로 base 의 uid 집합만 enrich 한다 (value 1 = 활성).
	if s, err := h.querier.Query(ctx, "kube_pod_status_phase"); err == nil {
		for _, sm := range s {
			if sm.Value == 1 {
				if p, ok := byUID[sm.Labels["uid"]]; ok {
					p.Phase = sm.Labels["phase"]
				}
			}
		}
	}
	if s, err := h.querier.Query(ctx, "kube_pod_status_qos_class"); err == nil {
		for _, sm := range s {
			if sm.Value == 1 {
				if p, ok := byUID[sm.Labels["uid"]]; ok {
					p.QOSClass = sm.Labels["qos_class"]
				}
			}
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
