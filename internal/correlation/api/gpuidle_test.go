package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	h := NewSynthesisHandler(q, nil, nil)
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
	h := NewSynthesisHandler(q, nil, nil)
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

	h := NewSynthesisHandler(q, nil, nil)
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

// TestGpuIdle_Pod_TieBreaker 는 top cause weight 가 동률인 victim 들이 namespace·pod 사전순으로
// 결정적으로 정렬되는지 검증한다. sort.Slice 가 unstable 이라 타이브레이커가 없으면 순서가 비결정적이다.
func TestGpuIdle_Pod_TieBreaker(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("pod:gpu_idle_cause_weight:5m",
			sample(0.5, "node", "gpu", "victim_namespace", "default", "victim_pod", "bbb", "cause", "memory_pressure"),
			sample(0.5, "node", "gpu", "victim_namespace", "default", "victim_pod", "aaa", "cause", "memory_pressure")).
		on("gpu_idle_cause_weight:5m", sample(0.5, "cause", "memory_pressure"))

	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=pod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuIdleResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Victims) != 2 || resp.Victims[0].Pod != "aaa" || resp.Victims[1].Pod != "bbb" {
		t.Errorf("victims=%+v want 동률 시 aaa, bbb 순 (사전순 타이브레이커)", resp.Victims)
	}
}

// TestGpuIdle_Node 는 scope=node 가 노드별 cause weight 순위와 dominant cause 를 돌려주고, node
// 파라미터가 exact = 매처로 PromQL 에 결합되는지 검증한다.
func TestGpuIdle_Node(t *testing.T) {
	q := (&fakeQuerier{}).
		// node 규칙을 cluster 규칙보다 먼저 둬 "node:gpu_idle_cause_weight:5m" 가 먼저 매칭되게 한다.
		on("node:gpu_idle_cause_weight:5m",
			sample(0.7, "node", "gpu", "cause", "memory_pressure"),
			sample(0.3, "node", "gpu", "cause", "network_pressure")).
		on("node:gpu_idle_dominant_cause:5m",
			sample(1.0, "node", "gpu", "cause", "memory_pressure")).
		on("gpu_idle_cause_weight:5m", sample(0.7, "cause", "memory_pressure")).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu"))

	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=node&node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuIdleResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.NodeAttributions) != 1 {
		t.Fatalf("node_attributions=%d want 1", len(resp.NodeAttributions))
	}
	n := resp.NodeAttributions[0]
	if n.Node != "gpu" || n.DominantCause != "memory_pressure" || len(n.Causes) != 2 || n.Causes[0].Cause != "memory_pressure" {
		t.Errorf("node attribution=%+v want gpu/memory_pressure dominant", n)
	}
	// node 파라미터가 exact = 매처와 %q 로 결합됐는지 확인 (=~ 정규식 매처 미사용).
	if !q.sawQuery(`node:gpu_idle_cause_weight:5m{node="gpu"}`) {
		t.Errorf("exact = 매처 결합 쿼리 미확인: %v", q.queries)
	}
	if q.sawQuery("=~") {
		t.Errorf("정규식 매처(=~)가 쓰임: %v", q.queries)
	}
}

// TestGpuIdle_NodeParamIgnoredOutsideNodeScope 는 node 필터가 scope=node 전용임을 검증한다. scope=
// cluster 에서 node 파라미터를 줘도 공통 필드 resp.Nodes 가 특정 노드로 좁혀지지 않아야 (Cluster
// 와의 불일치 방지) 한다.
func TestGpuIdle_NodeParamIgnoredOutsideNodeScope(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu"), sample(0.8, "node", "worker1")).
		on("gpu_idle_cause_weight:5m", sample(0.6, "cause", "memory_pressure"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=cluster&node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuIdleResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Nodes) != 2 {
		t.Errorf("nodes=%d want 2 (scope=cluster 에서 node 필터 무시)", len(resp.Nodes))
	}
	// node selector 가 PromQL 에 결합되지 않아야 한다.
	if q.sawQuery(`{node="gpu"}`) {
		t.Errorf("scope=cluster 인데 node selector 결합됨: %v", q.queries)
	}
}

// TestGpuIdle_InvalidNode 는 DNS-1123 위반 node 값이 PromQL 결합 전에 400 으로 거부되는지 검증한다.
func TestGpuIdle_InvalidNode(t *testing.T) {
	for _, bad := range []string{`gpu"} or up{`, "UPPER", "node;drop", "a/b", "-lead"} {
		q := &fakeQuerier{}
		h := NewSynthesisHandler(q, nil, nil)
		rec := httptest.NewRecorder()
		target := "/api/v1/gpu-idle?scope=node&node=" + url.QueryEscape(bad)
		h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("node=%q status=%d want 400", bad, rec.Code)
		}
		if len(q.queries) != 0 {
			t.Errorf("node=%q 거부 후 쿼리 실행됨: %v (PromQL 결합 전 차단이어야 함)", bad, q.queries)
		}
	}
}

// TestGpuIdle_InvalidScope 는 알 수 없는 scope 에 400 을 돌려주는지 검증한다.
func TestGpuIdle_InvalidScope(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=foo", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid scope)", rec.Code)
	}
}

// TestGpuIdle_NilQuerier 는 querier 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestGpuIdle_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
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

// TestGpuIdle_NodeIgnoredMarker는 #447의 무시 표기를 검증한다. scope=cluster에 node를 주면
// 필터는 적용되지 않고 node_ignored=true가 실리며, scope=node에서는 실리지 않는다.
func TestGpuIdle_NodeIgnoredMarker(t *testing.T) {
	q := (&fakeQuerier{}).on("node:gpu_idle:5m", sample(0.5, "node", "n1"))
	h := NewSynthesisHandler(q, nil, nil)

	rec := httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=cluster&node=n1", nil))
	var resp GpuIdleResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.NodeIgnored {
		t.Errorf("scope=cluster + node 에서 node_ignored=true 여야 함")
	}

	rec = httptest.NewRecorder()
	h.GetGpuIdle(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-idle?scope=node&node=n1", nil))
	resp = GpuIdleResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.NodeIgnored {
		t.Errorf("scope=node 에서는 node_ignored 미표기여야 함")
	}
}
