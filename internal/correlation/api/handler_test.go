package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"netobs/internal/correlation"
)

type fakeSource struct {
	data          []correlation.NoisyNeighbor
	crossNode     []correlation.NodeInterference
	serviceImpact []correlation.ServiceImpact
	crossLevel    []correlation.CrossLevel
	impactGraph   correlation.ImpactGraph
	impactPaths   []correlation.ImpactPath
}

func (f *fakeSource) Snapshot() []correlation.NoisyNeighbor              { return f.data }
func (f *fakeSource) CrossNodeSnapshot() []correlation.NodeInterference  { return f.crossNode }
func (f *fakeSource) ServiceImpactSnapshot() []correlation.ServiceImpact { return f.serviceImpact }
func (f *fakeSource) CrossLevelSnapshot() []correlation.CrossLevel       { return f.crossLevel }
func (f *fakeSource) ImpactGraphSnapshot() correlation.ImpactGraph       { return f.impactGraph }
func (f *fakeSource) ImpactPathsSnapshot() []correlation.ImpactPath      { return f.impactPaths }

func newFakeCrossLevel(node string, dir correlation.CrossLevelDirection, ns, pod string, dim correlation.ResourceDimension, rank int, score float64) correlation.CrossLevel {
	return correlation.CrossLevel{
		Node:         node,
		Direction:    dir,
		PodNamespace: ns,
		Pod:          pod,
		Dimension:    dim,
		Rank:         rank,
		Score:        score,
	}
}

func newFakeServiceImpact(victimNS, victimWorkload, suspectNode string, dim correlation.ResourceDimension, rank int, score float64) correlation.ServiceImpact {
	return correlation.ServiceImpact{
		VictimNamespace: victimNS,
		VictimWorkload:  victimWorkload,
		SuspectNode:     suspectNode,
		Dimension:       dim,
		Rank:            rank,
		Score:           score,
	}
}

func newFakeNeighbor(victimNS, victimPod, suspectNS, suspectPod string, dim correlation.ResourceDimension, rank int) correlation.NoisyNeighbor {
	return correlation.NoisyNeighbor{
		Victim:    correlation.PodIdentity{Namespace: victimNS, Pod: victimPod},
		Suspect:   correlation.PodIdentity{Namespace: suspectNS, Pod: suspectPod},
		Dimension: dim,
		Rank:      rank,
		Score:     0.8,
	}
}

func newFakeNodeInterference(victimNode, suspectNode string, dim correlation.ResourceDimension, rank int, score float64) correlation.NodeInterference {
	return correlation.NodeInterference{
		VictimNode:  victimNode,
		SuspectNode: suspectNode,
		Dimension:   dim,
		Rank:        rank,
		Score:       score,
	}
}

func TestListNoisyNeighbors_HappyPath(t *testing.T) {
	source := &fakeSource{data: []correlation.NoisyNeighbor{
		newFakeNeighbor("ns-a", "victim-1", "ns-b", "suspect-1", correlation.DimensionCPU, 1),
		newFakeNeighbor("ns-a", "victim-2", "ns-b", "suspect-2", correlation.DimensionNetwork, 1),
		newFakeNeighbor("ns-c", "victim-3", "ns-b", "suspect-3", correlation.DimensionGPU, 2),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/noisy-neighbor", nil)
	w := httptest.NewRecorder()
	h.ListNoisyNeighbors(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", w.Code, w.Body.String())
	}
	var resp NoisyNeighborListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Page.Total != 3 {
		t.Errorf("total=%d want 3", resp.Page.Total)
	}
	if len(resp.Items) != 3 {
		t.Errorf("items=%d want 3", len(resp.Items))
	}
}

func TestListNoisyNeighbors_DimensionFilter(t *testing.T) {
	source := &fakeSource{data: []correlation.NoisyNeighbor{
		newFakeNeighbor("ns-a", "victim-1", "ns-b", "suspect-1", correlation.DimensionCPU, 1),
		newFakeNeighbor("ns-a", "victim-2", "ns-b", "suspect-2", correlation.DimensionNetwork, 1),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/noisy-neighbor?dimension=cpu", nil)
	w := httptest.NewRecorder()
	h.ListNoisyNeighbors(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp NoisyNeighborListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 {
		t.Errorf("total=%d want 1 (cpu filter)", resp.Page.Total)
	}
}

func TestListNoisyNeighbors_VictimSignalFilter(t *testing.T) {
	mk := func(pod string, sig correlation.VictimSignal) correlation.NoisyNeighbor {
		n := newFakeNeighbor("ns-a", pod, "ns-b", "suspect", correlation.DimensionCPU, 1)
		n.VictimSignal = sig
		return n
	}
	source := &fakeSource{data: []correlation.NoisyNeighbor{
		mk("v-lat", correlation.SignalLatency),
		mk("v-tput", correlation.SignalThroughput),
		mk("v-err", correlation.SignalError),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/noisy-neighbor?victim_signal=throughput", nil)
	w := httptest.NewRecorder()
	h.ListNoisyNeighbors(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp NoisyNeighborListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 {
		t.Errorf("total=%d want 1 (victim_signal=throughput)", resp.Page.Total)
	}
	if len(resp.Items) == 1 && resp.Items[0].VictimSignal != correlation.SignalThroughput {
		t.Errorf("victim_signal=%s want throughput", resp.Items[0].VictimSignal)
	}
}

func TestListNoisyNeighbors_InvalidVictimSignal(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/noisy-neighbor?victim_signal=bogus", nil)
	w := httptest.NewRecorder()
	h.ListNoisyNeighbors(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid victim_signal)", w.Code)
	}
}

func TestListNoisyNeighbors_InvalidDimension(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/noisy-neighbor?dimension=invalid", nil)
	w := httptest.NewRecorder()
	h.ListNoisyNeighbors(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestListNoisyNeighbors_Pagination(t *testing.T) {
	data := make([]correlation.NoisyNeighbor, 5)
	for i := range data {
		data[i] = newFakeNeighbor("ns", "victim", "ns", "suspect", correlation.DimensionCPU, i+1)
	}
	source := &fakeSource{data: data}
	h := NewHandler(source, source, source, source, source)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/noisy-neighbor?limit=2&offset=1", nil)
	w := httptest.NewRecorder()
	h.ListNoisyNeighbors(w, req)
	var resp NoisyNeighborListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 5 {
		t.Errorf("total=%d want 5", resp.Page.Total)
	}
	if len(resp.Items) != 2 {
		t.Errorf("items=%d want 2", len(resp.Items))
	}
	if resp.Items[0].Rank != 2 {
		t.Errorf("first rank=%d want 2 (offset=1)", resp.Items[0].Rank)
	}
}

func TestListCrossNode_HappyPath(t *testing.T) {
	source := &fakeSource{crossNode: []correlation.NodeInterference{
		newFakeNodeInterference("gpu", "ebpf-worker1", correlation.DimensionCPU, 1, 0.82),
		newFakeNodeInterference("gpu", "ebpf-worker2", correlation.DimensionNetwork, 1, 0.55),
		newFakeNodeInterference("ebpf-worker1", "gpu", correlation.DimensionGPU, 2, 0.31),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-node-interference", nil)
	w := httptest.NewRecorder()
	h.ListCrossNode(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", w.Code, w.Body.String())
	}
	var resp CrossNodeListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Page.Total != 3 {
		t.Errorf("total=%d want 3", resp.Page.Total)
	}
	if len(resp.Items) != 3 {
		t.Errorf("items=%d want 3", len(resp.Items))
	}
}

func TestListCrossNode_VictimNodeAndDimensionFilter(t *testing.T) {
	source := &fakeSource{crossNode: []correlation.NodeInterference{
		newFakeNodeInterference("gpu", "ebpf-worker1", correlation.DimensionCPU, 1, 0.82),
		newFakeNodeInterference("gpu", "ebpf-worker2", correlation.DimensionNetwork, 1, 0.55),
		newFakeNodeInterference("ebpf-worker1", "gpu", correlation.DimensionCPU, 1, 0.41),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-node-interference?victim_node=gpu&dimension=cpu", nil)
	w := httptest.NewRecorder()
	h.ListCrossNode(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp CrossNodeListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 {
		t.Errorf("total=%d want 1 (victim_node=gpu + dimension=cpu)", resp.Page.Total)
	}
	if len(resp.Items) == 1 && resp.Items[0].SuspectNode != "ebpf-worker1" {
		t.Errorf("suspect_node=%s want ebpf-worker1", resp.Items[0].SuspectNode)
	}
}

func TestListCrossNode_InvalidDimension(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-node-interference?dimension=invalid", nil)
	w := httptest.NewRecorder()
	h.ListCrossNode(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestListCrossNode_Pagination(t *testing.T) {
	data := make([]correlation.NodeInterference, 5)
	for i := range data {
		data[i] = newFakeNodeInterference("gpu", "ebpf-worker1", correlation.DimensionCPU, i+1, 0.9-float64(i)*0.1)
	}
	source := &fakeSource{crossNode: data}
	h := NewHandler(source, source, source, source, source)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-node-interference?limit=2&offset=1", nil)
	w := httptest.NewRecorder()
	h.ListCrossNode(w, req)
	var resp CrossNodeListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 5 {
		t.Errorf("total=%d want 5", resp.Page.Total)
	}
	if len(resp.Items) != 2 {
		t.Errorf("items=%d want 2", len(resp.Items))
	}
	if resp.Items[0].Rank != 2 {
		t.Errorf("first rank=%d want 2 (offset=1)", resp.Items[0].Rank)
	}
}

func TestListCrossNode_NilSourceGracefulEmpty(t *testing.T) {
	h := NewHandler(&fakeSource{}, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-node-interference", nil)
	w := httptest.NewRecorder()
	h.ListCrossNode(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (nil source graceful empty)", w.Code)
	}
	// 응답 body 가 "items": null 이 아닌 "items": [] 형태 인지 raw body 단언 으로 확인 한다.
	// resp.Items 의 nil/non-nil 은 unmarshal 후 동일 슬라이스 값 (nil 또는 []) 으로 lost 되어
	// JSON wire format 단에서 직접 검증 해야 graceful empty 정책 회귀 가 차단 된다.
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("body=%s, want items:[] in wire format (nil source graceful empty)", w.Body.String())
	}
	var resp CrossNodeListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 0 {
		t.Errorf("total=%d want 0", resp.Page.Total)
	}
	if resp.Items == nil {
		t.Errorf("items is nil, want empty slice []")
	}
}

func TestListServiceImpact_HappyPath(t *testing.T) {
	source := &fakeSource{serviceImpact: []correlation.ServiceImpact{
		newFakeServiceImpact("ns-a", "api", "ebpf-worker1", correlation.DimensionCPU, 1, 0.82),
		newFakeServiceImpact("ns-a", "web", "ebpf-worker2", correlation.DimensionNetwork, 1, 0.55),
		newFakeServiceImpact("ns-b", "batch", "gpu", correlation.DimensionGPU, 2, 0.31),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/service-impact", nil)
	w := httptest.NewRecorder()
	h.ListServiceImpact(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", w.Code, w.Body.String())
	}
	var resp ServiceImpactListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Page.Total != 3 {
		t.Errorf("total=%d want 3", resp.Page.Total)
	}
	if len(resp.Items) != 3 {
		t.Errorf("items=%d want 3", len(resp.Items))
	}
}

func TestListServiceImpact_VictimWorkloadAndDimensionFilter(t *testing.T) {
	source := &fakeSource{serviceImpact: []correlation.ServiceImpact{
		newFakeServiceImpact("ns-a", "api", "ebpf-worker1", correlation.DimensionCPU, 1, 0.82),
		newFakeServiceImpact("ns-a", "api", "ebpf-worker2", correlation.DimensionNetwork, 1, 0.55),
		newFakeServiceImpact("ns-a", "web", "gpu", correlation.DimensionCPU, 1, 0.41),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/service-impact?victim_workload=api&dimension=cpu", nil)
	w := httptest.NewRecorder()
	h.ListServiceImpact(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp ServiceImpactListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 {
		t.Errorf("total=%d want 1 (victim_workload=api + dimension=cpu)", resp.Page.Total)
	}
	if len(resp.Items) == 1 && resp.Items[0].SuspectNode != "ebpf-worker1" {
		t.Errorf("suspect_node=%s want ebpf-worker1", resp.Items[0].SuspectNode)
	}
}

func TestListServiceImpact_InvalidDimension(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/service-impact?dimension=invalid", nil)
	w := httptest.NewRecorder()
	h.ListServiceImpact(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestListServiceImpact_NilSourceGracefulEmpty(t *testing.T) {
	h := NewHandler(&fakeSource{}, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/service-impact", nil)
	w := httptest.NewRecorder()
	h.ListServiceImpact(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (nil source graceful empty)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("body=%s, want items:[] in wire format (nil source graceful empty)", w.Body.String())
	}
	var resp ServiceImpactListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 0 {
		t.Errorf("total=%d want 0", resp.Page.Total)
	}
	if resp.Items == nil {
		t.Errorf("items is nil, want empty slice []")
	}
}

func TestListCrossLevel_HappyPath(t *testing.T) {
	source := &fakeSource{crossLevel: []correlation.CrossLevel{
		newFakeCrossLevel("ebpf-worker1", correlation.DirectionNodeToPod, "ns-a", "api-0", correlation.DimensionCPU, 1, 0.82),
		newFakeCrossLevel("ebpf-worker1", correlation.DirectionPodToNode, "ns-a", "batch-1", correlation.DimensionMemory, 1, 0.55),
		newFakeCrossLevel("gpu", correlation.DirectionNodeToPod, "ns-b", "web-2", correlation.DimensionGPU, 2, 0.31),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-level", nil)
	w := httptest.NewRecorder()
	h.ListCrossLevel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", w.Code, w.Body.String())
	}
	var resp CrossLevelListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Page.Total != 3 {
		t.Errorf("total=%d want 3", resp.Page.Total)
	}
	if len(resp.Items) != 3 {
		t.Errorf("items=%d want 3", len(resp.Items))
	}
}

func TestListCrossLevel_NodeDirectionDimensionFilter(t *testing.T) {
	source := &fakeSource{crossLevel: []correlation.CrossLevel{
		newFakeCrossLevel("ebpf-worker1", correlation.DirectionNodeToPod, "ns-a", "api-0", correlation.DimensionCPU, 1, 0.82),
		newFakeCrossLevel("ebpf-worker1", correlation.DirectionPodToNode, "ns-a", "api-0", correlation.DimensionCPU, 1, 0.40),
		newFakeCrossLevel("gpu", correlation.DirectionNodeToPod, "ns-b", "web-2", correlation.DimensionCPU, 1, 0.31),
	}}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-level?node=ebpf-worker1&direction=node_to_pod&dimension=cpu", nil)
	w := httptest.NewRecorder()
	h.ListCrossLevel(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp CrossLevelListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 {
		t.Errorf("total=%d want 1 (node + direction + dimension filter)", resp.Page.Total)
	}
	if len(resp.Items) == 1 && resp.Items[0].Pod != "api-0" {
		t.Errorf("pod=%s want api-0", resp.Items[0].Pod)
	}
}

func TestListCrossLevel_InvalidDirection(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-level?direction=bogus", nil)
	w := httptest.NewRecorder()
	h.ListCrossLevel(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid direction)", w.Code)
	}
}

func TestListCrossLevel_InvalidDimension(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-level?dimension=invalid", nil)
	w := httptest.NewRecorder()
	h.ListCrossLevel(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestListCrossLevel_NilSourceGracefulEmpty(t *testing.T) {
	h := NewHandler(&fakeSource{}, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cross-level", nil)
	w := httptest.NewRecorder()
	h.ListCrossLevel(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (nil source graceful empty)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"items":[]`) {
		t.Errorf("body=%s, want items:[] in wire format (nil source graceful empty)", w.Body.String())
	}
	var resp CrossLevelListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 0 {
		t.Errorf("total=%d want 0", resp.Page.Total)
	}
	if resp.Items == nil {
		t.Errorf("items is nil, want empty slice []")
	}
}

func TestGetImpactGraph_HappyPath(t *testing.T) {
	// suspect a → victim b, a → c. nodes a/b/c, edges 2.
	graph := correlation.BuildImpactGraph([]correlation.NoisyNeighbor{
		newFakeNeighbor("ns", "b", "ns", "a", correlation.DimensionCPU, 1),
		newFakeNeighbor("ns", "c", "ns", "a", correlation.DimensionMemory, 1),
	})
	source := &fakeSource{impactGraph: graph}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-graph", nil)
	w := httptest.NewRecorder()
	h.GetImpactGraph(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", w.Code, w.Body.String())
	}
	var resp ImpactGraphResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Summary.NodeCount != 3 || resp.Summary.EdgeCount != 2 {
		t.Errorf("summary nodes=%d edges=%d want 3/2", resp.Summary.NodeCount, resp.Summary.EdgeCount)
	}
	if len(resp.Nodes) != 3 || len(resp.Edges) != 2 {
		t.Errorf("nodes=%d edges=%d want 3/2", len(resp.Nodes), len(resp.Edges))
	}
}

func TestGetImpactGraph_Filter(t *testing.T) {
	// a→b (ns-a), c→d (ns-b). namespace=ns-a 필터 시 a→b 만.
	g := correlation.BuildImpactGraph([]correlation.NoisyNeighbor{
		newFakeNeighbor("ns-a", "b", "ns-a", "a", correlation.DimensionCPU, 1),
		newFakeNeighbor("ns-b", "d", "ns-b", "c", correlation.DimensionCPU, 1),
	})
	source := &fakeSource{impactGraph: g}
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-graph?namespace=ns-a", nil)
	w := httptest.NewRecorder()
	h.GetImpactGraph(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp ImpactGraphResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Summary.EdgeCount != 1 {
		t.Errorf("edge_count=%d want 1 (namespace=ns-a 필터)", resp.Summary.EdgeCount)
	}
}

func TestGetImpactGraph_InvalidMinScore(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-graph?min_score=abc", nil)
	w := httptest.NewRecorder()
	h.GetImpactGraph(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid min_score)", w.Code)
	}
}

func TestGetImpactGraph_NilSourceGracefulEmpty(t *testing.T) {
	h := NewHandler(&fakeSource{}, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-graph", nil)
	w := httptest.NewRecorder()
	h.GetImpactGraph(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (nil source graceful empty)", w.Code)
	}
	// nil 그래프가 nodes:[] / edges:[] wire format 으로 정규화되는지 직접 단언한다.
	if !strings.Contains(w.Body.String(), `"nodes":[]`) || !strings.Contains(w.Body.String(), `"edges":[]`) {
		t.Errorf("body=%s, want nodes:[] edges:[] (graceful empty)", w.Body.String())
	}
	var resp ImpactGraphResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Summary.NodeCount != 0 || resp.Summary.EdgeCount != 0 {
		t.Errorf("summary=%+v want 0/0", resp.Summary)
	}
	if resp.Nodes == nil || resp.Edges == nil {
		t.Errorf("nodes/edges nil, want empty slices")
	}
}

// pathsFakeSource 는 a→b, a→c 그래프에서 경로와 root 를 채운 fakeSource 를 만든다.
func pathsFakeSource() *fakeSource {
	g := correlation.BuildImpactGraph([]correlation.NoisyNeighbor{
		newFakeNeighbor("ns", "b", "ns", "a", correlation.DimensionCPU, 1),
		newFakeNeighbor("ns", "c", "ns", "a", correlation.DimensionCPU, 1),
	})
	paths, _ := correlation.ExtractImpactPaths(g, 5, 0.5, 1024)
	return &fakeSource{impactPaths: paths}
}

func TestListImpactPaths_HappyPath(t *testing.T) {
	source := pathsFakeSource()
	h := NewHandler(source, source, source, source, source)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-paths", nil)
	w := httptest.NewRecorder()
	h.ListImpactPaths(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200, body=%s", w.Code, w.Body.String())
	}
	var resp ImpactPathsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Summary.PathCount != 2 || resp.Summary.RootCount != 1 {
		t.Errorf("summary paths=%d roots=%d want 2/1", resp.Summary.PathCount, resp.Summary.RootCount)
	}
	if len(resp.Roots) == 1 && resp.Roots[0].Reach != 2 {
		t.Errorf("root reach=%d want 2", resp.Roots[0].Reach)
	}
}

func TestListImpactPaths_TerminalFilter(t *testing.T) {
	source := pathsFakeSource()
	h := NewHandler(source, source, source, source, source)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-paths?terminal_pod=b", nil)
	w := httptest.NewRecorder()
	h.ListImpactPaths(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp ImpactPathsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Summary.PathCount != 1 {
		t.Errorf("path_count=%d want 1 (terminal_pod=b)", resp.Summary.PathCount)
	}
	if len(resp.Paths) == 1 && resp.Paths[0].Terminal.Pod != "b" {
		t.Errorf("terminal=%s want b", resp.Paths[0].Terminal.Pod)
	}
}

// TestListImpactPaths_RootsRecomputedFromFiltered 는 필터로 경로를 좁히면 roots summary 도 그 부분집합
// 에서 재집계돼 reach 가 줄고 paths 와 정합하는지 검증한다. terminal_pod=b 면 a→b 경로 1 개만 남으므로
// 근원 a 의 reach 는 전역 2 가 아닌 1 이어야 한다.
func TestListImpactPaths_RootsRecomputedFromFiltered(t *testing.T) {
	source := pathsFakeSource()
	h := NewHandler(source, source, source, source, source)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-paths?terminal_pod=b", nil)
	w := httptest.NewRecorder()
	h.ListImpactPaths(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp ImpactPathsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Summary.PathCount != 1 || resp.Summary.RootCount != 1 {
		t.Fatalf("summary paths=%d roots=%d want 1/1", resp.Summary.PathCount, resp.Summary.RootCount)
	}
	if len(resp.Roots) != 1 || resp.Roots[0].Reach != 1 || resp.Roots[0].PathCount != 1 {
		t.Errorf("filtered root=%+v want reach=1 path_count=1 (전역 2 가 아닌 필터 부분집합)", resp.Roots)
	}
}

func TestListImpactPaths_InvalidMinScore(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{}, &fakeSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-paths?min_score=abc", nil)
	w := httptest.NewRecorder()
	h.ListImpactPaths(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid min_score)", w.Code)
	}
}

func TestListImpactPaths_NilSourceGracefulEmpty(t *testing.T) {
	h := NewHandler(&fakeSource{}, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/impact-paths", nil)
	w := httptest.NewRecorder()
	h.ListImpactPaths(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (nil source graceful empty)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"paths":[]`) || !strings.Contains(w.Body.String(), `"roots":[]`) {
		t.Errorf("body=%s, want paths:[] roots:[] (graceful empty)", w.Body.String())
	}
	var resp ImpactPathsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Summary.PathCount != 0 || resp.Summary.RootCount != 0 {
		t.Errorf("summary=%+v want 0/0", resp.Summary)
	}
	if resp.Paths == nil || resp.Roots == nil {
		t.Errorf("paths/roots nil, want empty slices")
	}
}
