package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func bandwidthFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("sum by(node, src_namespace, src_pod, direction, layer)",
			sample(1000, "node", "n1", "src_namespace", "a", "src_pod", "p1", "direction", "ingress", "layer", "l4"),
			sample(2000, "node", "n1", "src_namespace", "a", "src_pod", "p1", "direction", "egress", "layer", "l4"),
			sample(2200, "node", "n1", "src_namespace", "a", "src_pod", "p1", "direction", "egress", "layer", "nic"),
			sample(500, "node", "n1", "src_namespace", "b", "src_pod", "p2", "direction", "egress", "layer", "l4")).
		on("sum by(node, direction, layer)",
			sample(1000, "node", "n1", "direction", "ingress", "layer", "l4"),
			sample(2500, "node", "n1", "direction", "egress", "layer", "l4"),
			sample(2200, "node", "n1", "direction", "egress", "layer", "nic")).
		on("netobs_node_nic_capacity_bytes_per_sec",
			sample(10000, "node", "n1"))
}

// TestBandwidth 는 pod 별 direction×layer 병합 (RX=l4 ingress, TX=l4 egress, NIC TX 별도), 합산
// 내림차순 정렬, node 합계와 capacity 대비 NIC 사용률 산출을 검증한다.
func TestBandwidth(t *testing.T) {
	h := NewSynthesisHandler(bandwidthFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetBandwidth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bandwidth", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp BandwidthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pods) != 2 || resp.Pods[0].Pod != "p1" {
		t.Fatalf("pods=%+v want p1(3000) 먼저", resp.Pods)
	}
	p := resp.Pods[0]
	if p.RxBytesPerSec != 1000 || p.TxBytesPerSec != 2000 {
		t.Errorf("p1 rx/tx=%v/%v want 1000/2000", p.RxBytesPerSec, p.TxBytesPerSec)
	}
	if p.NicTxBytesPerSec == nil || *p.NicTxBytesPerSec != 2200 {
		t.Errorf("p1 nic_tx=%v want 2200", p.NicTxBytesPerSec)
	}
	if resp.Pods[1].NicTxBytesPerSec != nil {
		t.Errorf("p2 nic_tx=%v want nil (nic 미수집 pod)", resp.Pods[1].NicTxBytesPerSec)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("nodes=%+v want n1 1개", resp.Nodes)
	}
	n := resp.Nodes[0]
	if n.RxBytesPerSec != 1000 || n.TxBytesPerSec != 2500 {
		t.Errorf("n1 rx/tx=%v/%v want 1000/2500", n.RxBytesPerSec, n.TxBytesPerSec)
	}
	if n.NicCapacityBytesPerSec == nil || *n.NicCapacityBytesPerSec != 10000 {
		t.Errorf("n1 capacity=%v want 10000", n.NicCapacityBytesPerSec)
	}
	if n.NicUtilization == nil || *n.NicUtilization != 0.22 {
		t.Errorf("n1 nic_utilization=%v want 0.22 (2200/10000)", n.NicUtilization)
	}
}

// TestBandwidth_NamespaceFilter 는 ?namespace 가 %q 이스케이프로 pod 쿼리 matcher 에 반영되는지
// 검증한다. node 합계 쿼리는 필터와 무관해야 한다.
func TestBandwidth_NamespaceFilter(t *testing.T) {
	fq := bandwidthFakeQuerier()
	h := NewSynthesisHandler(fq, nil, nil)
	rec := httptest.NewRecorder()
	h.GetBandwidth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bandwidth?namespace=a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !fq.sawQuery(`{src_namespace="a"}`) {
		t.Errorf("pod 쿼리에 src_namespace matcher 미반영: %v", fq.queries)
	}
	for _, q := range fq.queries {
		if q == `sum by(node, direction, layer) (rate(netobs_pod_bytes_total[5m]))` {
			return
		}
	}
	t.Errorf("node 합계 쿼리가 무필터가 아님: %v", fq.queries)
}

// TestBandwidth_QueryError 는 주 소스 (pod 대역폭) 쿼리 실패 시 500 을 돌려주는지 검증한다.
func TestBandwidth_QueryError(t *testing.T) {
	h := NewSynthesisHandler(errQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetBandwidth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bandwidth", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// TestBandwidth_NilQuerier 는 querier 미주입 시 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestBandwidth_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetBandwidth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bandwidth", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp BandwidthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pods) != 0 || len(resp.Nodes) != 0 {
		t.Errorf("resp=%+v want empty", resp)
	}
}
