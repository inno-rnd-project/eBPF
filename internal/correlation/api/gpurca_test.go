package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"netobs/internal/correlation"
)

func gpuRcaQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("node:gpu_idle_cause_weight:5m",
			sample(0.7, "node", "gpu", "cause", "memory_pressure"),
			sample(0.2, "node", "gpu", "cause", "network_pressure")).
		on("node:gpu_idle_dominant_cause:5m", sample(1.0, "node", "gpu", "cause", "memory_pressure")).
		on("kube_pod_info",
			sample(1, "node", "gpu", "namespace", "ns1", "pod", "trainer"),
			sample(1, "node", "gpu", "namespace", "ns1", "pod", "sidecar")).
		on("ALERTS", sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "src_namespace", "ns2", "src_pod", "hog"))
}

// TestNodeGpuRca 는 dominant cause 와 신뢰도 (top1-top2 margin), noisy-neighbor/cross-node suspect
// 집계, narrative 합성을 검증한다. victim 이 이 노드에 사는 noisy-neighbor 만 잡히고, cross-node 는
// victim_node 매칭으로 잡힌다.
func TestNodeGpuRca(t *testing.T) {
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{
		// victim trainer 는 gpu 노드 pod → 채택. suspect hog 에 alert 매칭 (ns2/hog).
		{
			Victim:    correlation.PodIdentity{Namespace: "ns1", Pod: "trainer"},
			Suspect:   correlation.PodIdentity{Namespace: "ns2", Pod: "hog"},
			Dimension: correlation.DimensionNetwork, Score: 0.88,
		},
		// victim other 는 gpu 노드에 없음 → 제외.
		{
			Victim:    correlation.PodIdentity{Namespace: "ns9", Pod: "other"},
			Suspect:   correlation.PodIdentity{Namespace: "ns9", Pod: "x"},
			Dimension: correlation.DimensionCPU, Score: 0.99,
		},
	}}
	cn := &fakeCrossNode{data: []correlation.NodeInterference{
		{VictimNode: "gpu", SuspectNode: "worker1", Dimension: correlation.DimensionNetwork, Score: 0.5},
		{VictimNode: "worker2", SuspectNode: "worker1", Dimension: correlation.DimensionCPU, Score: 0.95}, // 다른 victim_node → 제외
	}}
	h := NewSynthesisHandler(gpuRcaQuerier(), nb, cn)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeGpuRcaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DominantCause != "memory_pressure" {
		t.Errorf("dominant=%q want memory_pressure", resp.DominantCause)
	}
	// 신뢰도 = top1(0.7) - top2(0.2) = 0.5.
	if resp.Confidence < 0.499 || resp.Confidence > 0.501 {
		t.Errorf("confidence=%v want ~0.5 (margin)", resp.Confidence)
	}
	// suspect 는 noisy-neighbor(hog, 0.88) + cross-node(worker1, 0.5), score 내림차순.
	if len(resp.Suspects) != 2 {
		t.Fatalf("suspects=%d want 2: %+v", len(resp.Suspects), resp.Suspects)
	}
	if resp.Suspects[0].Source != "noisy_neighbor" || resp.Suspects[0].Pod != "hog" {
		t.Errorf("suspects[0]=%+v want noisy_neighbor hog", resp.Suspects[0])
	}
	if len(resp.Suspects[0].Issues) != 1 || resp.Suspects[0].Issues[0] != "NetObsDropBurst" {
		t.Errorf("suspects[0] issues=%v want [NetObsDropBurst]", resp.Suspects[0].Issues)
	}
	if resp.Suspects[1].Source != "cross_node" || resp.Suspects[1].Node != "worker1" {
		t.Errorf("suspects[1]=%+v want cross_node worker1", resp.Suspects[1])
	}
}

// TestNodeGpuRca_DedupSuspect 는 동일 suspect pod 가 dimension/signal 별로 여러 페어를 가질 때
// 후보당 1 건 (최고 score) 으로 집계되는지 검증한다.
func TestNodeGpuRca_DedupSuspect(t *testing.T) {
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{
		{Victim: correlation.PodIdentity{Namespace: "ns1", Pod: "trainer"}, Suspect: correlation.PodIdentity{Namespace: "ns2", Pod: "hog"}, Dimension: correlation.DimensionNetwork, Score: 0.3},
		{Victim: correlation.PodIdentity{Namespace: "ns1", Pod: "trainer"}, Suspect: correlation.PodIdentity{Namespace: "ns2", Pod: "hog"}, Dimension: correlation.DimensionMemory, Score: 0.8},
		{Victim: correlation.PodIdentity{Namespace: "ns1", Pod: "sidecar"}, Suspect: correlation.PodIdentity{Namespace: "ns2", Pod: "hog"}, Dimension: correlation.DimensionCPU, Score: 0.5},
	}}
	h := NewSynthesisHandler(gpuRcaQuerier(), nb, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Suspects) != 1 {
		t.Fatalf("suspects=%d want 1 (동일 suspect pod dedup): %+v", len(resp.Suspects), resp.Suspects)
	}
	if resp.Suspects[0].Pod != "hog" || resp.Suspects[0].Score != 0.8 {
		t.Errorf("suspect=%+v want hog 최고 score 0.8", resp.Suspects[0])
	}
}

// TestNodeGpuRca_NamespaceAware 는 동명 pod 가 다른 namespace 에 있을 때 victim 매칭이 namespace 를
// 함께 대조해 오귀속하지 않는지 검증한다. kube_pod_info 는 ns1/trainer 만 이 노드에 두고, noisy-
// neighbor victim 은 ns-other/trainer (동명, 다른 ns) 라 제외돼야 한다.
func TestNodeGpuRca_NamespaceAware(t *testing.T) {
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{
		{
			Victim:    correlation.PodIdentity{Namespace: "ns-other", Pod: "trainer"},
			Suspect:   correlation.PodIdentity{Namespace: "ns2", Pod: "hog"},
			Dimension: correlation.DimensionNetwork, Score: 0.9,
		},
	}}
	h := NewSynthesisHandler(gpuRcaQuerier(), nb, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Suspects) != 0 {
		t.Errorf("suspects=%+v want 0 (ns-other/trainer 는 이 노드 pod 아님)", resp.Suspects)
	}
}

// TestNodeGpuRca_MissingNode 는 node 파라미터 누락 시 400 을 검증한다.
func TestNodeGpuRca_MissingNode(t *testing.T) {
	h := NewSynthesisHandler(gpuRcaQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (node 누락)", rec.Code)
	}
}

// TestNodeGpuRca_InvalidNode 는 DNS-1123 위반 node 값이 PromQL 결합 전에 400 으로 거부되는지 검증한다.
func TestNodeGpuRca_InvalidNode(t *testing.T) {
	q := gpuRcaQuerier()
	q.queries = nil
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	target := "/api/v1/gpu-rca?node=" + url.QueryEscape(`gpu"} or up{`)
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v (PromQL 결합 전 차단이어야 함)", q.queries)
	}
}

// TestNodeGpuRca_NotIdle 은 cause 귀속이 비면 (유휴 게이팅 미충족) narrative 가 그 사실을 적고
// confidence 가 0 인지 검증한다.
func TestNodeGpuRca_NotIdle(t *testing.T) {
	q := (&fakeQuerier{}).on("node:gpu_idle:5m", sample(0.2, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.DominantCause != "" || resp.Confidence != 0 {
		t.Errorf("resp=%+v want dominant 없음/confidence 0", resp)
	}
}
