package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"netobs/internal/correlation"
)

type fakeSource struct {
	data []correlation.NoisyNeighbor
}

func (f *fakeSource) Snapshot() []correlation.NoisyNeighbor { return f.data }

func newFakeNeighbor(victimNS, victimPod, suspectNS, suspectPod string, dim correlation.ResourceDimension, rank int) correlation.NoisyNeighbor {
	return correlation.NoisyNeighbor{
		Victim:    correlation.PodIdentity{Namespace: victimNS, Pod: victimPod},
		Suspect:   correlation.PodIdentity{Namespace: suspectNS, Pod: suspectPod},
		Dimension: dim,
		Rank:      rank,
		Score:     0.8,
	}
}

func TestListNoisyNeighbors_HappyPath(t *testing.T) {
	source := &fakeSource{data: []correlation.NoisyNeighbor{
		newFakeNeighbor("ns-a", "victim-1", "ns-b", "suspect-1", correlation.DimensionCPU, 1),
		newFakeNeighbor("ns-a", "victim-2", "ns-b", "suspect-2", correlation.DimensionNetwork, 1),
		newFakeNeighbor("ns-c", "victim-3", "ns-b", "suspect-3", correlation.DimensionGPU, 2),
	}}
	h := NewHandler(source)

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
	h := NewHandler(source)

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
	h := NewHandler(&fakeSource{})
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
	h := NewHandler(&fakeSource{data: data})
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
