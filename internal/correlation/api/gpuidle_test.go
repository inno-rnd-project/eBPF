package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGpuIdle_Cluster 는 scope=cluster 가 노드별 유휴 비율과 cluster 원인 가중치 순위, dominant cause
// 를 합성하는지 검증한다. thermal(0.0) 처럼 weight 0 인 신규 원인도 cause-generic 하게 노출된다.
func TestGpuIdle_Cluster(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu"), sample(0.1, "node", "worker1")).
		on("gpu_idle_cause_weight:5m",
			sample(0.7, "cause", "memory_pressure"),
			sample(0.2, "cause", "network_pressure"),
			sample(0.0, "cause", "thermal")).
		on("cluster:gpu_idle_dominant_cause:5m", sample(1.000005, "cause", "memory_pressure"))

	h := NewSynthesisHandler(q, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuIdleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Scope != "cluster" {
		t.Errorf("scope=%q want cluster", resp.Scope)
	}
	if len(resp.Nodes) != 2 || resp.Nodes[0].Node != "gpu" || resp.Nodes[0].Severity != "high" {
		t.Errorf("nodes=%+v want gpu(0.9/high) 먼저", resp.Nodes)
	}
	if resp.Cluster == nil || resp.Cluster.DominantCause != "memory_pressure" {
		t.Fatalf("cluster=%+v want dominant memory_pressure", resp.Cluster)
	}
	if len(resp.Cluster.Causes) != 3 || resp.Cluster.Causes[0].Cause != "memory_pressure" || resp.Cluster.Causes[0].Weight != 0.7 {
		t.Errorf("causes=%+v want memory_pressure 0.7 먼저, thermal 포함 3건", resp.Cluster.Causes)
	}
	if !strings.Contains(resp.Summary, "memory_pressure") {
		t.Errorf("summary=%q want memory_pressure 언급", resp.Summary)
	}
}

// TestGpuIdle_NotIdle 는 cause weight 가 없으면 (GPU idle 게이팅 미충족) cluster 가 null 로 graceful
// 처리되는지 검증한다.
func TestGpuIdle_NotIdle(t *testing.T) {
	q := (&fakeQuerier{}).on("node:gpu_idle:5m", sample(0.2, "node", "gpu"))
	h := NewSynthesisHandler(q, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuIdleResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Cluster != nil {
		t.Errorf("cluster=%+v want nil (idle 게이팅 미충족)", resp.Cluster)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Severity != "low" {
		t.Errorf("nodes=%+v want gpu(0.2/low)", resp.Nodes)
	}
	if !strings.Contains(resp.Summary, "임계") {
		t.Errorf("summary=%q want 임계 미만 언급", resp.Summary)
	}
}

// TestGpuIdle_Pod 는 scope=pod 가 victim Pod 단위 원인 귀속을 dominant cause 와 함께 돌려주는지 검증한다.
func TestGpuIdle_Pod(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		// pod 규칙을 cluster 규칙보다 먼저 둬 "pod:gpu_idle_cause_weight:5m" 가 먼저 매칭되게 한다.
		on("pod:gpu_idle_cause_weight:5m",
			sample(0.6, "node", "gpu", "victim_namespace", "default", "victim_pod", "trainer", "cause", "memory_pressure"),
			sample(0.1, "node", "gpu", "victim_namespace", "default", "victim_pod", "trainer", "cause", "network_pressure")).
		on("victim:gpu_idle_dominant_cause:5m",
			sample(1.0, "node", "gpu", "victim_namespace", "default", "victim_pod", "trainer", "cause", "memory_pressure")).
		on("gpu_idle_cause_weight:5m", sample(0.6, "cause", "memory_pressure"))

	h := NewSynthesisHandler(q, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=pod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuIdleResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Victims) != 1 {
		t.Fatalf("victims=%d want 1", len(resp.Victims))
	}
	v := resp.Victims[0]
	if v.Namespace != "default" || v.Pod != "trainer" || v.Node != "gpu" {
		t.Errorf("victim=%+v want default/trainer/gpu", v)
	}
	if v.DominantCause != "memory_pressure" || len(v.Causes) != 2 || v.Causes[0].Cause != "memory_pressure" {
		t.Errorf("victim cause=%+v want memory_pressure dominant", v)
	}
}

// TestGpuIdle_InvalidScope 는 알 수 없는 scope 에 400 을 돌려주는지 검증한다.
func TestGpuIdle_InvalidScope(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=foo", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid scope)", rec.Code)
	}
}

// TestGpuIdle_NilQuerier 는 querier 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestGpuIdle_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuIdleResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Nodes) != 0 || resp.Cluster != nil {
		t.Errorf("resp=%+v want empty nodes/nil cluster (nil querier)", resp)
	}
}
