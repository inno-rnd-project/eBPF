package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"netobs/internal/correlation"
)

// TestInventory_Nodes 는 /api/v1/nodes 가 kube_node_* 메트릭을 합쳐 노드 기본 정보 (uid / 내부·외부 IP /
// Ready / 버전 / capacity) 를 노출하는지 검증한다.
func TestInventory_Nodes(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_node_info", sample(1, "node", "gpu", "system_uuid", "uuid-1", "internal_ip", "10.0.0.21",
			"kubelet_version", "v1.35.3", "container_runtime_version", "docker://28", "kernel_version", "6.2", "os_image", "Ubuntu 22.04")).
		on("kube_node_status_addresses", sample(1, "node", "gpu", "address", "1.2.3.4", "type", "ExternalIP")).
		on("kube_node_status_condition", sample(1, "node", "gpu", "condition", "Ready", "status", "true")).
		on("kube_node_status_capacity",
			sample(8, "node", "gpu", "resource", "cpu"),
			sample(67e9, "node", "gpu", "resource", "memory"),
			sample(1, "node", "gpu", "resource", "nvidia_com_gpu")).
		on("kube_node_role", sample(1, "node", "gpu", "role", "control-plane"))

	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes=%d want 1", len(resp.Nodes))
	}
	n := resp.Nodes[0]
	if n.Name != "gpu" || n.UID != "uuid-1" || n.InternalIP != "10.0.0.21" || n.ExternalIP != "1.2.3.4" || !n.Ready || n.KubeletVersion != "v1.35.3" {
		t.Errorf("node=%+v want gpu/uuid-1/10.0.0.21/1.2.3.4/ready/v1.35.3", n)
	}
	if n.Capacity.CPU == nil || *n.Capacity.CPU != 8 || n.Capacity.GPU == nil || *n.Capacity.GPU != 1 || n.Capacity.MemoryBytes == nil {
		t.Errorf("capacity=%+v want cpu 8/gpu 1/memory 설정", n.Capacity)
	}
	// #248 역할: kube_node_role 라벨이 병합된다.
	if len(n.Roles) != 1 || n.Roles[0] != "control-plane" {
		t.Errorf("roles=%v want [control-plane]", n.Roles)
	}
}

// TestInventory_Pods 는 /api/v1/pods 가 kube_pod_info 에 phase / qos 를 uid join 으로 합쳐 namespace·pod
// 사전순으로 돌려주는지 검증한다.
func TestInventory_Pods(t *testing.T) {
	q := podFakeQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPods(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp PodsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pods) != 2 || resp.Pods[0].Namespace != "default" || resp.Pods[1].Namespace != "kube-system" {
		t.Fatalf("pods=%+v want default, kube-system 순", resp.Pods)
	}
	p := resp.Pods[0]
	if p.Pod != "trainer" || p.UID != "u1" || p.PodIP != "10.1.1.1" || p.Node != "gpu" ||
		p.WorkloadKind != "ReplicaSet" || p.WorkloadName != "trainer-abc" || p.Phase != "Running" || p.QOSClass != "Burstable" {
		t.Errorf("pod=%+v want trainer 완전 enrich", p)
	}
	// #248 관측 커버리지: netobs 시리즈가 있는 trainer 만 observed, coredns 는 no-data.
	if !resp.Pods[0].Observed {
		t.Errorf("trainer observed=false want true (netobs 시리즈 존재)")
	}
	if resp.Pods[1].Observed {
		t.Errorf("coredns observed=true want false (no-data)")
	}
}

// TestInventory_Pods_NamespaceFilter 는 ?namespace 필터가 해당 namespace 만 돌려주는지 검증한다.
func TestInventory_Pods_NamespaceFilter(t *testing.T) {
	h := NewSynthesisHandler(podFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetPods(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pods?namespace=default", nil))
	var resp PodsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pods) != 1 || resp.Pods[0].Pod != "trainer" {
		t.Errorf("pods=%+v want default/trainer 1건", resp.Pods)
	}
}

// TestInventory_NilQuerier 는 querier 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestInventory_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	for _, path := range []string{"/api/v1/nodes", "/api/v1/pods"} {
		rec := httptest.NewRecorder()
		if path == "/api/v1/nodes" {
			h.GetNodes(rec, httptest.NewRequest(http.MethodGet, path, nil))
		} else {
			h.GetPods(rec, httptest.NewRequest(http.MethodGet, path, nil))
		}
		if rec.Code != http.StatusOK {
			t.Errorf("%s status=%d want 200", path, rec.Code)
		}
	}
}

func podFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("kube_pod_info",
			sample(1, "namespace", "default", "pod", "trainer", "uid", "u1", "pod_ip", "10.1.1.1", "host_ip", "10.0.0.21", "node", "gpu", "created_by_kind", "ReplicaSet", "created_by_name", "trainer-abc"),
			sample(1, "namespace", "kube-system", "pod", "coredns", "uid", "u2", "pod_ip", "10.1.1.2", "host_ip", "10.0.0.27", "node", "master", "created_by_kind", "ReplicaSet", "created_by_name", "coredns-xyz")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u1", "phase", "Running"),
			sample(0, "uid", "u1", "phase", "Pending"),
			sample(1, "uid", "u2", "phase", "Running")).
		on("kube_pod_status_qos_class", sample(1, "uid", "u1", "qos_class", "Burstable")).
		on("netobs_pod_bytes_total", sample(3, "src_namespace", "default", "src_pod", "trainer"))
}

// TestListPagination_OptIn 은 #411 의 opt-in 페이지네이션을 고정한다. 미지정 요청은 종전대로 전량이고
// page 필드가 생략되며, limit/offset 지정 시 그만큼 절단되고 total 은 절단 전 개수다. 형식 위반은
// 400 이다.
func TestListPagination_OptIn(t *testing.T) {
	samples := []correlation.InstantSample{}
	for i := 0; i < 5; i++ {
		samples = append(samples, sample(1, "node", fmt.Sprintf("n%d", i)))
	}
	q := (&fakeQuerier{}).on("kube_node_info", samples...)
	h := NewSynthesisHandler(q, nil, nil)

	get := func(target string) NodesResponse {
		rec := httptest.NewRecorder()
		h.GetNodes(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d want 200", target, rec.Code)
		}
		var resp NodesResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	full := get("/api/v1/nodes")
	if len(full.Nodes) != 5 || full.Page != nil {
		t.Errorf("미지정 요청 nodes=%d page=%v want 5/nil", len(full.Nodes), full.Page)
	}
	paged := get("/api/v1/nodes?limit=2&offset=1")
	if len(paged.Nodes) != 2 {
		t.Fatalf("절단 nodes=%d want 2", len(paged.Nodes))
	}
	if paged.Nodes[0].Name != "n1" {
		t.Errorf("offset 반영 실패: 첫 항목=%s want n1", paged.Nodes[0].Name)
	}
	if paged.Page == nil || paged.Page.Total != 5 || paged.Page.Limit != 2 || paged.Page.Offset != 1 {
		t.Errorf("page=%+v want limit2/offset1/total5", paged.Page)
	}
	// 범위 밖 offset 은 빈 목록이고 total 은 유지된다.
	over := get("/api/v1/nodes?offset=99")
	if len(over.Nodes) != 0 || over.Page == nil || over.Page.Total != 5 {
		t.Errorf("범위 밖 offset nodes=%d page=%+v", len(over.Nodes), over.Page)
	}

	rec := httptest.NewRecorder()
	h.GetNodes(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nodes?limit=-1", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit=-1 status=%d want 400", rec.Code)
	}
}
