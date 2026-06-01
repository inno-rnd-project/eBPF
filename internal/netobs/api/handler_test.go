package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeFlowSource struct{ data []FlowEntry }

func (f *fakeFlowSource) SnapshotFlows() []FlowEntry { return f.data }

type fakeDropSource struct{ data []DropEntry }

func (d *fakeDropSource) SnapshotDrops() []DropEntry { return d.data }

func TestListFlows_NilSource(t *testing.T) {
	h := NewHandler(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil)
	w := httptest.NewRecorder()
	h.ListFlows(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status=%d want 200 (graceful empty)", w.Code)
	}
	var resp FlowListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 0 {
		t.Errorf("total=%d want 0", resp.Page.Total)
	}
}

func TestListFlows_ProtocolFilter(t *testing.T) {
	h := NewHandler(&fakeFlowSource{data: []FlowEntry{
		{Protocol: "tcp", SrcPod: "p1"},
		{Protocol: "udp", SrcPod: "p2"},
	}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/flows?protocol=tcp", nil)
	w := httptest.NewRecorder()
	h.ListFlows(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp FlowListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 || resp.Items[0].SrcPod != "p1" {
		t.Errorf("filter mismatch: total=%d items=%v", resp.Page.Total, resp.Items)
	}
}

func TestListFlows_InvalidProtocol(t *testing.T) {
	h := NewHandler(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/flows?protocol=icmp", nil)
	w := httptest.NewRecorder()
	h.ListFlows(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", w.Code)
	}
}

func TestListDrops_ReasonFilter(t *testing.T) {
	h := NewHandler(nil, &fakeDropSource{data: []DropEntry{
		{DropReason: "TCP_INVALID_SEQUENCE", SrcPod: "p1"},
		{DropReason: "PKT_TOO_SMALL", SrcPod: "p2"},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/drops?drop_reason=PKT_TOO_SMALL", nil)
	w := httptest.NewRecorder()
	h.ListDrops(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var resp DropListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Page.Total != 1 || resp.Items[0].SrcPod != "p2" {
		t.Errorf("filter mismatch: %v", resp.Items)
	}
}
