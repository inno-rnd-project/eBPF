package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"netobs/internal/correlation"
)

// #381 노드·pod 상태 어휘 단일 규약의 API 간 정합 테스트다. 각 핸들러가 방출하는 상태 값이 규약
// enum 에 속하는지, node/{node} 의 기존 status 가 status_unified 의 고정 환원 매핑과 항상
// 일치하는지, 같은 pressure 가 topology 와 node/{node} 에서 같은 규약 어휘로 나오는지 본다.

var unifiedNodeStatuses = map[string]bool{
	correlation.NodeStatusHealthy:  true,
	correlation.NodeStatusWarning:  true,
	correlation.NodeStatusCritical: true,
	correlation.NodeStatusDown:     true,
	correlation.NodeStatusUnknown:  true,
}

// TestVocabulary_NodeStatusSubset 은 overview·node-map 공유 nodeStatus 가 규약 어휘의 3단
// rollup (healthy/warning/down, critical 은 warning 으로 압축) 만 방출하는지 검증한다.
func TestVocabulary_NodeStatusSubset(t *testing.T) {
	cases := []struct {
		ready, alerted bool
		pressure       float64
		want           string
	}{
		{false, false, 0, correlation.NodeStatusDown},
		{true, true, 0, correlation.NodeStatusWarning},
		{true, false, 0.9, correlation.NodeStatusWarning},
		{true, false, 0.1, correlation.NodeStatusHealthy},
	}
	for _, c := range cases {
		got := nodeStatus(c.ready, c.alerted, c.pressure)
		if got != c.want {
			t.Errorf("nodeStatus(%v,%v,%v) = %s, want %s", c.ready, c.alerted, c.pressure, got, c.want)
		}
		if !unifiedNodeStatuses[got] {
			t.Errorf("nodeStatus 가 규약 밖 어휘 %q 를 방출한다", got)
		}
	}
}

// TestVocabulary_PodStatusLifecycle 은 node-map pod 판정이 lifecycle 축 어휘만 방출하는지 phase
// 전수로 검증한다.
func TestVocabulary_PodStatusLifecycle(t *testing.T) {
	lifecycle := map[string]bool{
		correlation.PodStatusLive:      true,
		correlation.PodStatusWarning:   true,
		correlation.PodStatusDown:      true,
		correlation.PodStatusCompleted: true,
	}
	for _, phase := range []string{"Running", "Pending", "Succeeded", "Failed", "Unknown", ""} {
		for _, hasIssue := range []bool{false, true} {
			if got := podStatus(phase, hasIssue); !lifecycle[got] {
				t.Errorf("podStatus(%q,%v) = %q 가 lifecycle 축 밖이다", phase, hasIssue, got)
			}
		}
	}
}

// TestVocabulary_GetNode_StatusUnifiedMapping 은 node/{node} 가 규약 어휘 status_unified 를 싣고
// 기존 status 가 그 고정 환원 (correlation.NodeDetailStatus) 과 항상 일치하는지 등급별로 검증한다.
func TestVocabulary_GetNode_StatusUnifiedMapping(t *testing.T) {
	cases := []struct {
		name        string
		cpu         float64 // 음수면 시리즈 자체를 싣지 않는다 (신호 부재 unknown 케이스)
		wantUnified string
		wantStatus  string
	}{
		{"healthy-ok", 0.1, correlation.NodeStatusHealthy, "ok"},
		{"warning-warn", 0.5, correlation.NodeStatusWarning, "warn"},
		{"critical-degraded", 0.8, correlation.NodeStatusCritical, "degraded"},
		{"no-data-unknown", -1, correlation.NodeStatusUnknown, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{}
			if tc.cpu >= 0 {
				q = q.on("node:cpu_pressure_score", sample(tc.cpu, "node", "worker2"))
			}
			h := NewSynthesisHandler(q, nil, nil)
			rec := httptest.NewRecorder()
			h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker2", nil))
			var resp NodeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.StatusUnified != tc.wantUnified || resp.Status != tc.wantStatus {
				t.Errorf("status_unified=%q status=%q want %s/%s", resp.StatusUnified, resp.Status, tc.wantUnified, tc.wantStatus)
			}
			if resp.Status != correlation.NodeDetailStatus(resp.StatusUnified) {
				t.Errorf("status %q 가 status_unified %q 의 환원 매핑과 불일치한다", resp.Status, resp.StatusUnified)
			}
		})
	}
}

// TestVocabulary_TopologyGetNodeSameGrade 는 같은 cpu pressure 입력이 topology 의 status 와
// node/{node} 의 status_unified 에서 같은 규약 어휘로 나오는지 검증한다 (API 간 정합).
func TestVocabulary_TopologyGetNodeSameGrade(t *testing.T) {
	for _, tc := range []struct {
		cpu  float64
		want string
	}{
		{0.8, correlation.NodeStatusCritical},
		{0.5, correlation.NodeStatusWarning},
		{0.1, correlation.NodeStatusHealthy},
	} {
		q := (&fakeQuerier{}).on("node:cpu_pressure_score", sample(tc.cpu, "node", "worker1"))
		h := NewSynthesisHandler(q, nil, nil)

		rec := httptest.NewRecorder()
		h.GetTopology(rec, httptest.NewRequest(http.MethodGet, "/api/v1/topology", nil))
		var topo TopologyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &topo); err != nil {
			t.Fatalf("decode topology: %v", err)
		}

		rec = httptest.NewRecorder()
		h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker1", nil))
		var node NodeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil {
			t.Fatalf("decode node: %v", err)
		}

		if len(topo.Nodes) != 1 || topo.Nodes[0].Status != tc.want || node.StatusUnified != tc.want {
			t.Errorf("cpu %.2f: topology=%+v node.status_unified=%q want 둘 다 %s", tc.cpu, topo.Nodes, node.StatusUnified, tc.want)
		}
	}
}
