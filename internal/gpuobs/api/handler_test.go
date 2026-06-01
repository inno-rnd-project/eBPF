package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeGPUSource struct{ data []GPUEntry }

func (f *fakeGPUSource) SnapshotGPU(scope string) []GPUEntry {
	result := make([]GPUEntry, 0, len(f.data))
	for _, e := range f.data {
		if e.Scope == scope {
			result = append(result, e)
		}
	}
	return result
}

func TestListGPU_DefaultScope(t *testing.T) {
	h := NewHandler(&fakeGPUSource{data: []GPUEntry{
		{Scope: "device", GPUUUID: "u1", UtilizationPercent: 50},
		{Scope: "pod", GPUUUID: "u1", SrcPod: "p1"},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/gpu", nil)
	w := httptest.NewRecorder()
	h.ListGPU(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp GPUListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 || resp.Items[0].Scope != "device" {
		t.Errorf("default scope=device expected, got %v", resp.Items)
	}
}

func TestListGPU_InvalidScope(t *testing.T) {
	h := NewHandler(&fakeGPUSource{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/gpu?scope=invalid", nil)
	w := httptest.NewRecorder()
	h.ListGPU(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestListGPU_PodScopeFilter(t *testing.T) {
	h := NewHandler(&fakeGPUSource{data: []GPUEntry{
		{Scope: "pod", SrcNamespace: "ns-a", SrcPod: "p1"},
		{Scope: "pod", SrcNamespace: "ns-a", SrcPod: "p2"},
		{Scope: "pod", SrcNamespace: "ns-b", SrcPod: "p3"},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/gpu?scope=pod&src_namespace=ns-a", nil)
	w := httptest.NewRecorder()
	h.ListGPU(w, req)
	var resp GPUListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 2 {
		t.Errorf("total=%d want 2", resp.Page.Total)
	}
}

func TestListGPU_NilSource(t *testing.T) {
	h := NewHandler(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/gpu", nil)
	w := httptest.NewRecorder()
	h.ListGPU(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d", w.Code)
	}
}
