package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func nodeMapFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("kube_node_info",
			sample(1, "node", "gpu"),
			sample(1, "node", "worker1")).
		on("kube_node_status_condition",
			sample(1, "node", "gpu", "condition", "Ready", "status", "true"),
			sample(1, "node", "worker1", "condition", "Ready", "status", "true")).
		on("kube_node_role", sample(1, "node", "worker1", "role", "control-plane")).
		on("kube_node_status_capacity", sample(2, "node", "gpu", "resource", "nvidia_com_gpu")).
		on("node:pressure_score:5m", sample(0.1, "node", "gpu"), sample(0.1, "node", "worker1")).
		// trainer 를 가리키는 firing alert 2종 (src_pod 와 victim_pod 규약). gpu 노드는 node 라벨로 경고.
		on("ALERTS",
			sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "node", "gpu", "src_pod", "trainer"),
			sample(1, "alertname", "CorrelationStrongNoisyNeighbor", "severity", "warning", "victim_pod", "trainer")).
		on("kube_pod_info",
			sample(1, "namespace", "ns1", "pod", "trainer", "uid", "u1", "node", "gpu"),
			sample(1, "namespace", "ns1", "pod", "crashed", "uid", "u2", "node", "gpu"),
			sample(1, "namespace", "ns2", "pod", "web", "uid", "u3", "node", "worker1")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u1", "phase", "Running"),
			sample(1, "uid", "u2", "phase", "Failed"),
			sample(1, "uid", "u3", "phase", "Running")).
		on("netobs_pod_bytes_total", sample(3, "src_namespace", "ns1", "src_pod", "trainer"))
}

// TestNodeMap 은 노드 그리드 합성 (pod 수 내림차순 정렬, GPU 뱃지, 노드/pod 3단 상태, alert 매칭
// issues, no-data) 을 검증한다.
func TestNodeMap(t *testing.T) {
	h := NewSynthesisHandler(nodeMapFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeMapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 2 || resp.Nodes[0].Name != "gpu" {
		t.Fatalf("nodes=%+v want gpu (pod 2) 선두", resp.Nodes)
	}
	gpu := resp.Nodes[0]
	if !gpu.GPU || gpu.GPUDevices != 2 || gpu.Status != "warning" || gpu.PodCount != 2 {
		t.Errorf("gpu 노드=%+v want gpu/devices2/warning/pod2", gpu)
	}
	// pod 정렬은 namespace·pod 사전순: crashed 가 trainer 앞.
	if gpu.Pods[0].Pod != "crashed" || gpu.Pods[0].Status != "down" {
		t.Errorf("pods[0]=%+v want crashed down (phase Failed)", gpu.Pods[0])
	}
	trainer := gpu.Pods[1]
	if trainer.Status != "warning" || trainer.NoData {
		t.Errorf("trainer=%+v want warning (alert 매칭) + 관측 중", trainer)
	}
	// issues: 규약 2종 (src_pod, victim_pod) 매칭 alertname 이 사전순 dedup 으로 들어간다.
	if len(trainer.Issues) != 2 || trainer.Issues[0] != "CorrelationStrongNoisyNeighbor" || trainer.Issues[1] != "NetObsDropBurst" {
		t.Errorf("issues=%v want 2종 사전순", trainer.Issues)
	}
	worker := resp.Nodes[1]
	if worker.Status != "healthy" || len(worker.Roles) != 1 || worker.Pods[0].Status != "live" || !worker.Pods[0].NoData {
		t.Errorf("worker=%+v want healthy/roles1/live/no-data", worker)
	}
}

// TestNodeMap_NodeFilter 는 단일 노드 조회와 미등록 노드 404 를 검증한다.
func TestNodeMap_NodeFilter(t *testing.T) {
	h := NewSynthesisHandler(nodeMapFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?node=worker1", nil))
	var resp NodeMapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Name != "worker1" {
		t.Fatalf("nodes=%+v want worker1 단건", resp.Nodes)
	}
	rec = httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?node=bogus", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 (미등록 노드)", rec.Code)
	}
}

// TestNodeMap_AtParam 은 at 전파와 잘못된 at 의 400 을 검증한다.
func TestNodeMap_AtParam(t *testing.T) {
	q := nodeMapFakeQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?at=1751943600", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if q.lastAt.Unix() != 1751943600 {
		t.Errorf("lastAt=%v want unix 1751943600 전파", q.lastAt)
	}
	rec = httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?at=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}
