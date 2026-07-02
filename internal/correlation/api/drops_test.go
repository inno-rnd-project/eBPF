package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func dropsFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("netobs_drop_events_labeled_total",
			sample(2.5, "node", "w1", "src_namespace", "cs", "src_workload", "client", "direction", "egress", "drop_reason", "NOT_SPECIFIED", "drop_category", "unknown", "drop_stage", "unknown", "dst_namespace", "cs", "dst_workload", "client"),
			sample(0.3, "node", "gpu", "src_namespace", "gm", "src_workload", "dcgm", "direction", "egress", "drop_reason", "QUEUE_PURGE", "drop_category", "queue", "drop_stage", "egress_qdisc")).
		on("netobs_drop_events_flow_total",
			sample(1.2, "node", "gpu", "src_namespace", "ebpf-project", "src_pod", "p1", "direction", "egress", "drop_reason", "REASON_1", "drop_category", "unknown", "protocol", "TCP", "src_ip", "10.0.0.1", "src_port", "5000", "dst_ip", "10.0.0.2", "dst_port", "443", "ip_version", "4")).
		on("netobs_drop_last_timestamp_seconds",
			sample(1782700000, "node", "gpu", "protocol", "TCP", "src_ip", "10.0.0.1", "src_port", "5000", "dst_ip", "10.0.0.2", "dst_port", "443", "direction", "egress", "src_namespace", "ebpf-project", "src_pod", "p1")).
		on("netobs_drop_stack_total",
			sample(0.5, "drop_reason", "QUEUE_PURGE", "drop_category", "queue", "func", "__dev_queue_xmit"))
}

// TestDrops 는 labeled 기반 drop 랭킹과 opt-in flows(5-tuple + last_seen join), stacks 를 합성하는지 검증한다.
func TestDrops(t *testing.T) {
	h := NewSynthesisHandler(dropsFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetDrops(rec, httptest.NewRequest(http.MethodGet, "/api/v1/drops", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp DropsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Drops) != 2 || resp.Drops[0].Workload != "client" || resp.Drops[0].DropsPerSec != 2.5 {
		t.Fatalf("drops=%+v want client 2.5 먼저", resp.Drops)
	}
	if resp.Drops[0].Reason != "NOT_SPECIFIED" || resp.Drops[1].Reason != "QUEUE_PURGE" {
		t.Errorf("drop reasons=%+v", resp.Drops)
	}
	if resp.Drops[1].Stage != "egress_qdisc" {
		t.Errorf("drop stage=%q want egress_qdisc (QUEUE_PURGE)", resp.Drops[1].Stage)
	}
	if len(resp.Flows) != 1 || resp.Flows[0].Pod != "p1" || resp.Flows[0].SrcIP != "10.0.0.1" {
		t.Fatalf("flows=%+v want p1 5-tuple 1건", resp.Flows)
	}
	if resp.Flows[0].LastSeenUnix == nil || *resp.Flows[0].LastSeenUnix != 1782700000 {
		t.Errorf("flow last_seen=%v want 1782700000 (join)", resp.Flows[0].LastSeenUnix)
	}
	if len(resp.Stacks) != 1 || resp.Stacks[0].Func != "__dev_queue_xmit" {
		t.Errorf("stacks=%+v want __dev_queue_xmit", resp.Stacks)
	}
	if !resp.FlowDetailEnabled {
		t.Errorf("flow_detail_enabled=false want true (flows/stacks 존재)")
	}
}

// TestDrops_NamespaceFilter 는 ?namespace 필터가 src_namespace 로 거르는지 검증한다.
func TestDrops_NamespaceFilter(t *testing.T) {
	h := NewSynthesisHandler(dropsFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetDrops(rec, httptest.NewRequest(http.MethodGet, "/api/v1/drops?namespace=cs", nil))
	var resp DropsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Drops) != 1 || resp.Drops[0].Namespace != "cs" {
		t.Errorf("drops=%+v want cs 1건", resp.Drops)
	}
}

// TestDrops_QueryError 는 주 소스(labeled) 쿼리 실패 시 500 을 돌려주는지 검증한다.
func TestDrops_QueryError(t *testing.T) {
	h := NewSynthesisHandler(errQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetDrops(rec, httptest.NewRequest(http.MethodGet, "/api/v1/drops", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (labeled query 실패)", rec.Code)
	}
}

// TestDrops_NilQuerier 는 querier 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestDrops_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetDrops(rec, httptest.NewRequest(http.MethodGet, "/api/v1/drops", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp DropsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Drops) != 0 || resp.FlowDetailEnabled {
		t.Errorf("resp=%+v want empty/false (nil querier)", resp)
	}
}
