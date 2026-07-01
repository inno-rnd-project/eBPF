package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"netobs/internal/correlation"
)

// fakeCrossNode 는 CrossNodeSnapshotSource 테스트 더블이다.
type fakeCrossNode struct {
	data []correlation.NodeInterference
}

func (f *fakeCrossNode) CrossNodeSnapshot() []correlation.NodeInterference { return f.data }

// TestTopology 는 /api/v1/topology 가 노드별 dominant severity status 와 cross-node 간섭 엣지를 한
// 응답으로 합성하는지 검증한다.
func TestTopology(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score:5m", sample(0.8, "node", "worker1"), sample(0.5, "node", "worker2"), sample(0.1, "node", "gpu")).
		on("node:memory_pressure_score:5m", sample(0.45, "node", "gpu"), sample(0.2, "node", "worker2"))
	cn := &fakeCrossNode{data: []correlation.NodeInterference{
		{SuspectNode: "worker1", VictimNode: "gpu", Dimension: correlation.DimensionCPU, Score: 0.7},
	}}
	h := NewSynthesisHandler(q, nil, cn)
	rec := httptest.NewRecorder()
	h.GetTopology(rec, httptest.NewRequest(http.MethodGet, "/api/v1/topology", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp TopologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 3 || resp.Nodes[0].Node != "gpu" || resp.Nodes[1].Node != "worker1" || resp.Nodes[2].Node != "worker2" {
		t.Fatalf("nodes=%+v want gpu/worker1/worker2 정렬", resp.Nodes)
	}
	if resp.Nodes[1].Status != "critical" || resp.Nodes[1].DominantDimension != "cpu" {
		t.Errorf("worker1=%+v want critical/cpu", resp.Nodes[1])
	}
	if resp.Nodes[0].Status != "warning" || resp.Nodes[0].DominantDimension != "memory" {
		t.Errorf("gpu=%+v want warning/memory (dominant 0.45)", resp.Nodes[0])
	}
	if len(resp.Edges) != 1 || resp.Edges[0].SuspectNode != "worker1" || resp.Edges[0].VictimNode != "gpu" ||
		resp.Edges[0].Dimension != string(correlation.DimensionCPU) || resp.Edges[0].Score != 0.7 {
		t.Errorf("edges=%+v want worker1->gpu cpu 0.7", resp.Edges)
	}
	if !strings.Contains(resp.Summary, "critical 1") {
		t.Errorf("summary=%q want critical 1 언급", resp.Summary)
	}
}

// TestTopology_Empty 는 querier 와 crossNode 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestTopology_Empty(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetTopology(rec, httptest.NewRequest(http.MethodGet, "/api/v1/topology", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp TopologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 0 || len(resp.Edges) != 0 {
		t.Errorf("resp=%+v want empty nodes/edges", resp)
	}
}
