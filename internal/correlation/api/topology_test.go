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
	// #444 gpu 노드의 memory 0.45는 실측 사용률이라 usage 임계(0.85) 미만의 healthy다. 종전 raw
	// 판정은 pressure 임계(0.4)로 warning을 냈다. dominant는 severity 동률(low)에서 progress가 큰
	// memory(0.45/0.85)가 cpu(0.1/0.4)를 이긴다.
	if resp.Nodes[0].Status != "healthy" || resp.Nodes[0].DominantDimension != "memory" {
		t.Errorf("gpu=%+v want healthy/memory (usage 임계 정합)", resp.Nodes[0])
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

// TestTopology_MemoryUsageScaleAlignment는 #444의 척도 정합을 검증한다. memory 사용률 0.6의
// 건강한 노드가 topology에서 dominant=memory, status=warning으로 보이던 결함이 닫혔는지, cpu
// elevated(0.45)가 raw 값이 큰 memory 사용률(0.6)을 severity 우선으로 이기는지 단정한다.
// /health(pressureSeverityFor)와 /node(nodePressureGrade)가 이미 쓰는 규약과 동일하다.
func TestTopology_MemoryUsageScaleAlignment(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score:5m", sample(0.45, "node", "worker1")).
		on("node:memory_pressure_score:5m", sample(0.60, "node", "worker1"), sample(0.60, "node", "worker2"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetTopology(rec, httptest.NewRequest(http.MethodGet, "/api/v1/topology", nil))
	var resp TopologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// worker1: cpu 0.45는 elevated, memory 0.60은 usage 임계 미만 low라 cpu가 dominant다. 종전
	// raw 최대값 판정이면 memory(0.60)가 cpu(0.45)를 눌렀다.
	if resp.Nodes[0].DominantDimension != "cpu" || resp.Nodes[0].Status != "warning" {
		t.Errorf("worker1=%+v want warning/cpu (severity 우선)", resp.Nodes[0])
	}
	// worker2: memory 0.60 단독은 healthy다. 종전 판정이면 pressure 임계(0.4)로 warning이 났다.
	if resp.Nodes[1].DominantDimension != "memory" || resp.Nodes[1].Status != "healthy" {
		t.Errorf("worker2=%+v want healthy/memory (usage 임계)", resp.Nodes[1])
	}
}

// TestTopology_DominantDeterministicOnTie는 #444의 동률 결정성을 검증한다. 같은 severity와 같은
// severityProgress의 두 차원(cpu 0.2와 network 0.2, 둘 다 low에 progress 0.5)에서 dominant가
// 반복 호출에도 synthDimensions 순서의 앞선 차원으로 고정되는지 단정한다. 종전 map 순회는 순서가
// 비결정이라 요청마다 dominant가 달라질 수 있었다.
func TestTopology_DominantDeterministicOnTie(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score:5m", sample(0.2, "node", "n1")).
		on("node:network_pressure_score:5m", sample(0.2, "node", "n1"))
	h := NewSynthesisHandler(q, nil, nil)
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		h.GetTopology(rec, httptest.NewRequest(http.MethodGet, "/api/v1/topology", nil))
		var resp TopologyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Nodes) != 1 || resp.Nodes[0].DominantDimension != "cpu" {
			t.Fatalf("call %d: nodes=%+v want dominant=cpu 고정 (synthDimensions 선순위)", i+1, resp.Nodes)
		}
	}
}
