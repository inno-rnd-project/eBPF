package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func agentsFakeQuerier() *fakeQuerier {
	// on 은 contains 매칭이라 각 쿼리의 고유 metric 이름으로 규칙을 건다. up 은 netobs 3 노드 중
	// worker2 가 down, gpuobs 는 gpu 노드만 존재하는 dev 형태를 재현한다.
	return (&fakeQuerier{}).
		on("up{",
			sample(1, "node", "gpu", "job", "netobs-agent"),
			sample(1, "node", "worker1", "job", "netobs-agent"),
			sample(0, "node", "worker2", "job", "netobs-agent"),
			sample(1, "node", "gpu", "job", "gpuobs-agent")).
		on("sum by(node) (netobs_bpf_program_loaded",
			sample(26, "node", "gpu"),
			sample(24, "node", "worker1")).
		on("count by(node) (netobs_bpf_program_loaded",
			sample(26, "node", "gpu"),
			sample(26, "node", "worker1")).
		on("netobs_bpf_program_attach_total", sample(2, "node", "worker1")).
		on("gpuobs_nvml_errors_total", sample(2.5, "node", "gpu")).
		on("netobs_informer_sync_lag_seconds", sample(10, "node", "gpu"), sample(400, "node", "worker1")).
		on("gpuobs_informer_sync_lag_seconds", sample(5, "node", "gpu"))
}

// TestAgents 는 (node, agent) 항목 골격과 알림 규칙 동일 임계 판정 (bpf 미로드, attach 실패,
// nvml 오류율, informer stale, down) 을 검증한다.
func TestAgents(t *testing.T) {
	h := NewSynthesisHandler(agentsFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetAgents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp AgentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 4 {
		t.Fatalf("agents=%d want 4 (netobs 3 + gpuobs 1): %+v", len(resp.Agents), resp.Agents)
	}
	byKey := map[string]AgentHealth{}
	for _, a := range resp.Agents {
		byKey[a.Node+"/"+a.Agent] = a
	}
	// gpu/gpuobs: nvml 2.5/s > 1 → degraded GPUObsAgentNvmlErrorsHigh.
	g := byKey["gpu/gpuobs"]
	if g.Status != "degraded" || len(g.Issues) != 1 || g.Issues[0] != "GPUObsAgentNvmlErrorsHigh" {
		t.Errorf("gpu/gpuobs=%+v want GPUObsAgentNvmlErrorsHigh degraded", g)
	}
	// gpu/netobs: 전부 정상 → healthy.
	if n := byKey["gpu/netobs"]; n.Status != "healthy" || len(n.Issues) != 0 {
		t.Errorf("gpu/netobs=%+v want healthy", n)
	}
	// worker1/netobs: loaded 24 < total 26 + attach 실패 2 + lag 400 → issues 3종.
	w1 := byKey["worker1/netobs"]
	if w1.Status != "degraded" || len(w1.Issues) != 3 {
		t.Errorf("worker1/netobs=%+v want degraded issues 3종", w1)
	}
	// worker2/netobs: up==0 → ObsAgentDown.
	w2 := byKey["worker2/netobs"]
	if w2.Up || w2.Status != "degraded" || len(w2.Issues) != 1 || w2.Issues[0] != "ObsAgentDown" {
		t.Errorf("worker2/netobs=%+v want ObsAgentDown degraded", w2)
	}
}

// TestAgents_InvalidNode 는 DNS-1123 위반 node 가 쿼리 실행 전에 400 으로 거부되는지 검증한다.
func TestAgents_InvalidNode(t *testing.T) {
	q := agentsFakeQuerier()
	q.queries = nil
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	target := "/api/v1/agents?node=" + url.QueryEscape(`gpu"} or up{`)
	h.GetAgents(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v", q.queries)
	}
}

// TestAgents_NilQuerier 는 querier 미주입 시 빈 응답을 graceful 하게 돌려주는지 검증한다.
func TestAgents_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetAgents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp AgentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 0 {
		t.Errorf("agents=%d want 0", len(resp.Agents))
	}
}
