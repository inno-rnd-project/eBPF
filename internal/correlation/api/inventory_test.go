package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
			sample(1, "node", "gpu", "resource", "nvidia_com_gpu"))

	h := NewSynthesisHandler(q, nil)
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
}

// TestInventory_Pods 는 /api/v1/pods 가 kube_pod_info 에 phase / qos 를 uid join 으로 합쳐 namespace·pod
// 사전순으로 돌려주는지 검증한다.
func TestInventory_Pods(t *testing.T) {
	q := podFakeQuerier()
	h := NewSynthesisHandler(q, nil)
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
}

// TestInventory_Pods_NamespaceFilter 는 ?namespace 필터가 해당 namespace 만 돌려주는지 검증한다.
func TestInventory_Pods_NamespaceFilter(t *testing.T) {
	h := NewSynthesisHandler(podFakeQuerier(), nil)
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
	h := NewSynthesisHandler(nil, nil)
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
		on("kube_pod_status_qos_class", sample(1, "uid", "u1", "qos_class", "Burstable"))
}
