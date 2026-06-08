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
	data      []correlation.NoisyNeighbor
	crossNode []correlation.NodeInterference
}

func (f *fakeSource) Snapshot() []correlation.NoisyNeighbor             { return f.data }
func (f *fakeSource) CrossNodeSnapshot() []correlation.NodeInterference { return f.crossNode }

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
	h := NewHandler(source, source)

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
	h := NewHandler(source, source)

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

func TestListNoisyNeighbors_InvalidDimension(t *testing.T) {
	h := NewHandler(&fakeSource{}, &fakeSource{})
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
	h := NewHandler(source, source)
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
	h := NewHandler(source, source)

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
	h := NewHandler(source, source)

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
	h := NewHandler(&fakeSource{}, &fakeSource{})
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
	h := NewHandler(source, source)
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
	h := NewHandler(&fakeSource{}, nil)
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
