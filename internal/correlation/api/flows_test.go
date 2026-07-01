package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func flowsFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).on("netobs_flow_bytes_total",
		sample(1_250_000, "node", "gpu", "src_namespace", "correlation-stress", "src_workload", "client", "src_pod", "client-1",
			"dst_namespace", "ebpf-project", "dst_pod_uid", "u-server", "dst_ip", "10.1.0.2", "protocol", "TCP", "direction", "egress"),
		sample(100_000, "node", "gpu", "src_namespace", "ebpf-project", "src_workload", "correlation-exporter", "src_pod", "ce-1",
			"dst_namespace", "monitoring", "dst_pod_uid", "u-prom", "dst_ip", "10.1.0.3", "protocol", "TCP", "direction", "egress"))
}

// TestFlows 는 /api/v1/flows 가 flow_bytes rate 를 pod 간 대역폭 엣지(bytes/sec + Mbps)로 노출하고
// bytes 내림차순으로 정렬하는지 검증한다.
func TestFlows(t *testing.T) {
	h := NewSynthesisHandler(flowsFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetFlows(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp FlowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.FlowCollectionEnabled || len(resp.Edges) != 2 {
		t.Fatalf("edges=%+v enabled=%v want 2 edges/enabled", resp.Edges, resp.FlowCollectionEnabled)
	}
	e := resp.Edges[0]
	if e.SrcPod != "client-1" || e.DstNamespace != "ebpf-project" || e.DstPodUID != "u-server" || e.Protocol != "TCP" {
		t.Errorf("edge[0]=%+v want client-1 -> ebpf-project/u-server 먼저", e)
	}
	if e.BytesPerSec != 1_250_000 || e.Mbps != 10 {
		t.Errorf("edge[0] bytes=%v mbps=%v want 1.25e6 / 10Mbps", e.BytesPerSec, e.Mbps)
	}
}

// TestFlows_NamespaceFilter 는 ?namespace 필터가 PromQL label matcher(src_namespace="...")로 쿼리에
// 밀려 들어가는지 검증한다 (Prometheus 측 필터). fakeQuerier 는 셀렉터를 해석하지 않으므로 결과 개수
// 대신 생성된 쿼리 문자열을 확인한다.
func TestFlows_NamespaceFilter(t *testing.T) {
	q := flowsFakeQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetFlows(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows?namespace=ebpf-project", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !strings.Contains(q.lastQuery, `src_namespace="ebpf-project"`) {
		t.Errorf("query=%q want src_namespace 셀렉터 포함", q.lastQuery)
	}
}

// TestFlows_InvalidDirection 은 허용되지 않은 direction 에 400 을 돌려주는지 검증한다.
func TestFlows_InvalidDirection(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetFlows(rec, httptest.NewRequest(http.MethodGet, `/api/v1/flows?direction=x"}or(`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid direction)", rec.Code)
	}
}

// TestFlows_QueryError 는 쿼리 실패 시 500 을 돌려주는지 검증한다.
func TestFlows_QueryError(t *testing.T) {
	h := NewSynthesisHandler(errQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetFlows(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (query 실패)", rec.Code)
	}
}

// TestFlows_NilQuerier 는 querier 가 nil 일 때 panic 없이 빈 응답(flow_collection_enabled=false)을
// 돌려주는지 검증한다.
func TestFlows_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetFlows(rec, httptest.NewRequest(http.MethodGet, "/api/v1/flows", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp FlowsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Edges) != 0 || resp.FlowCollectionEnabled {
		t.Errorf("resp=%+v want empty/false (nil querier)", resp)
	}
}
