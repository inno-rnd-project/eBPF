package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func nodeResourcesQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("kube_node_status_capacity",
			sample(64, "node", "gpu", "resource", "cpu"),
			sample(1.35e11, "node", "gpu", "resource", "memory"),
			sample(110, "node", "gpu", "resource", "pods"),
			sample(1, "node", "gpu", "resource", "nvidia_com_gpu"),
			// 이슈 범위 밖 리소스는 무시된다.
			sample(5e11, "node", "gpu", "resource", "ephemeral_storage")).
		on("kube_node_status_allocatable",
			sample(64, "node", "gpu", "resource", "cpu"),
			sample(1.3e11, "node", "gpu", "resource", "memory"),
			sample(110, "node", "gpu", "resource", "pods"),
			sample(1, "node", "gpu", "resource", "nvidia_com_gpu")).
		on("kube_pod_container_resource_requests",
			sample(8.5, "resource", "cpu"),
			sample(2e10, "resource", "memory"),
			sample(1, "resource", "nvidia_com_gpu")).
		on("kube_pod_container_resource_limits",
			sample(16, "resource", "cpu"),
			sample(4e10, "resource", "memory")).
		on("container_cpu_usage_seconds_total", sample(3.2)).
		on("container_memory_working_set_bytes", sample(6.5e10)).
		on("count(kube_pod_info", sample(22)).
		on("gpuobs_device_utilization_percent", sample(40))
}

// TestNodeResources 는 리소스 4종의 capacity/allocatable/requests/limits 합성과 usage·활용률
// 산출, 범위 밖 리소스 무시, gpu 의 usage 생략과 사용률 활용률을 검증한다.
func TestNodeResources(t *testing.T) {
	h := NewSynthesisHandler(nodeResourcesQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeResources(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/gpu/resources", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeResourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Resources) != 4 {
		t.Fatalf("resources=%d want 4 (ephemeral 무시): %+v", len(resp.Resources), resp.Resources)
	}
	cpu := resp.Resources["cpu"]
	if cpu.Capacity == nil || *cpu.Capacity != 64 || cpu.Requests == nil || *cpu.Requests != 8.5 || cpu.Limits == nil || *cpu.Limits != 16 {
		t.Errorf("cpu=%+v want capacity 64/requests 8.5/limits 16", cpu)
	}
	if cpu.Usage == nil || *cpu.Usage != 3.2 || cpu.UtilizationRatio == nil || *cpu.UtilizationRatio != 0.05 {
		t.Errorf("cpu usage=%+v want 3.2 / ratio 0.05 (3.2/64)", cpu)
	}
	pods := resp.Resources["pods"]
	if pods.Usage == nil || *pods.Usage != 22 || pods.UtilizationRatio == nil || *pods.UtilizationRatio != 0.2 {
		t.Errorf("pods=%+v want usage 22 / ratio 0.2", pods)
	}
	if pods.Requests != nil || pods.Limits != nil {
		t.Errorf("pods requests/limits=%+v want 생략", pods)
	}
	gpu := resp.Resources["gpu"]
	if gpu.Capacity == nil || *gpu.Capacity != 1 || gpu.Requests == nil || *gpu.Requests != 1 {
		t.Errorf("gpu=%+v want capacity 1/requests 1", gpu)
	}
	if gpu.Usage != nil || gpu.UtilizationRatio == nil || *gpu.UtilizationRatio != 0.4 {
		t.Errorf("gpu=%+v want usage 생략 / ratio 0.4 (사용률 40%%)", gpu)
	}
	if !strings.Contains(resp.Summary, "cpu 활용률 5%") {
		t.Errorf("summary=%q want 활용률 요약", resp.Summary)
	}
}

// TestNodeResources_NoGpuNode 는 GPU 없는 노드에서 gpu 엔트리가 생략되는지 검증한다.
func TestNodeResources_NoGpuNode(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_node_status_capacity", sample(8, "node", "w1", "resource", "cpu")).
		on("kube_node_status_allocatable", sample(8, "node", "w1", "resource", "cpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeResources(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/w1/resources", nil))
	var resp NodeResourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := resp.Resources["gpu"]; ok {
		t.Errorf("resources=%+v want gpu 생략", resp.Resources)
	}
	if _, ok := resp.Resources["cpu"]; !ok {
		t.Errorf("resources=%+v want cpu 존재", resp.Resources)
	}
}

// TestNodeResources_Dispatch 는 nodeSubroute 가 세그먼트 수로 GetNode 와 리소스 현황과 400 을
// 분기하는지 검증한다.
func TestNodeResources_Dispatch(t *testing.T) {
	h := NewSynthesisHandler(nodeResourcesQuerier(), nil, nil)
	// {node}/resources → 리소스 현황.
	rec := httptest.NewRecorder()
	h.nodeSubroute(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/gpu/resources", nil))
	var resp NodeResourcesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Resources) == 0 {
		t.Errorf("dispatch resources 실패: code=%d err=%v", rec.Code, err)
	}
	// {node} 단독 → 기존 노드 상세 (NodeResponse 형태).
	rec = httptest.NewRecorder()
	h.nodeSubroute(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/gpu", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pressure"`) {
		t.Errorf("dispatch node 상세 실패: code=%d body=%s", rec.Code, rec.Body.String()[:80])
	}
	// 알 수 없는 하위 경로 → 400.
	rec = httptest.NewRecorder()
	h.nodeSubroute(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/gpu/unknown", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("dispatch unknown: code=%d want 400", rec.Code)
	}
}

// TestNodeResources_InvalidNode 는 DNS-1123 위반 노드가 쿼리 실행 전에 400 으로 거부되는지
// 검증한다.
func TestNodeResources_InvalidNode(t *testing.T) {
	q := &fakeQuerier{}
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeResources(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/BAD_NODE/resources", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v", q.queries)
	}
}
