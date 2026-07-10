package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func nodeVitalsQuerier() *fakeQuerier {
	// on 은 contains 매칭이라 각 쿼리의 고유 metric 이름으로 규칙을 건다.
	return (&fakeQuerier{}).
		on("container_cpu_usage_seconds_total", sample(4.1, "node", "gpu")).
		on("container_memory_working_set_bytes", sample(36.1, "node", "gpu")).
		on("gpuobs_device_utilization_percent", sample(5.0, "node", "gpu")).
		on("gpuobs_device_memory_used_bytes", sample(2.4e9, "node", "gpu")).
		on("gpuobs_device_memory_total_bytes", sample(24e9, "node", "gpu"))
}

// TestNodeVitals 는 노드 raw 사용률 (cpu/memory %, gpu %, gpu 메모리 used/total 과 파생 %) 을 한
// 응답으로 돌려주는지 검증한다.
func TestNodeVitals(t *testing.T) {
	h := NewSynthesisHandler(nodeVitalsQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeVitals(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-vitals?node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeVitalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CPUPercent == nil || *resp.CPUPercent != 4.1 {
		t.Errorf("cpu_percent=%v want 4.1", resp.CPUPercent)
	}
	if resp.MemoryPercent == nil || *resp.MemoryPercent != 36.1 {
		t.Errorf("memory_percent=%v want 36.1", resp.MemoryPercent)
	}
	if resp.GPUPercent == nil || *resp.GPUPercent != 5.0 {
		t.Errorf("gpu_percent=%v want 5.0", resp.GPUPercent)
	}
	// gpu 메모리 % = 2.4e9 / 24e9 * 100 = 10.
	if resp.GPUMemoryPercent == nil || *resp.GPUMemoryPercent < 9.99 || *resp.GPUMemoryPercent > 10.01 {
		t.Errorf("gpu_memory_percent=%v want 10", resp.GPUMemoryPercent)
	}
}

// TestNodeVitals_MissingNode 는 node 파라미터 누락 시 400 을 검증한다.
func TestNodeVitals_MissingNode(t *testing.T) {
	h := NewSynthesisHandler(nodeVitalsQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeVitals(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-vitals", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (node 누락)", rec.Code)
	}
}

// TestNodeVitals_InvalidNode 는 DNS-1123 위반 node 가 PromQL 결합 전에 400 으로 거부되는지 검증한다.
func TestNodeVitals_InvalidNode(t *testing.T) {
	q := nodeVitalsQuerier()
	q.queries = nil
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	target := "/api/v1/node-vitals?node=" + url.QueryEscape(`gpu"} or up{`)
	h.GetNodeVitals(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v", q.queries)
	}
}

// TestNodeVitals_Graceful 은 데이터 부재 시 사용률 필드가 생략되고 200 을 돌려주는지 검증한다.
func TestNodeVitals_Graceful(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeVitals(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-vitals?node=absent", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeVitalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CPUPercent != nil || resp.GPUMemoryPercent != nil {
		t.Errorf("resp=%+v want 사용률 생략 (데이터 부재)", resp)
	}
}
