package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gpuobstypes "netobs/internal/gpuobs/types"
)

// gpuProcessesAgent 는 gpuobs agent 의 /processes 를 흉내내는 httptest 서버를 만들고, up 시리즈의
// instance 라벨이 그 주소를 가리키는 fakeQuerier 와 함께 돌려준다.
func gpuProcessesAgent(t *testing.T, listing gpuobstypes.GPUProcessListing) (*httptest.Server, *fakeQuerier) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/processes" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(listing)
	}))
	t.Cleanup(srv.Close)
	instance := strings.TrimPrefix(srv.URL, "http://")
	// #409 포트 allow-list 에 httptest 의 임의 포트를 주입한다. 운영 값은 agent 고정 포트뿐이다.
	if _, port, err := net.SplitHostPort(instance); err == nil {
		agentAllowedPorts[port] = struct{}{}
		t.Cleanup(func() { delete(agentAllowedPorts, port) })
	}
	q := (&fakeQuerier{}).on(`up{job="gpuobs-agent"`, sample(1, "node", "gpu", "instance", instance))
	return srv, q
}

// TestGpuProcesses 는 up instance 해석 → agent 호출 → 목록 중계와 summary 의 pod 귀속 집계를
// 검증한다.
func TestGpuProcesses(t *testing.T) {
	sm := uint32(30)
	_, q := gpuProcessesAgent(t, gpuobstypes.GPUProcessListing{
		Node:        "gpu",
		CollectedAt: "2026-07-13T00:00:00Z",
		Processes: []gpuobstypes.GPUProcessDetail{
			{PID: 100, GpuUUID: "u0", Type: "compute", MemoryUsedBytes: 1024, SmUtilPercent: &sm, Namespace: "ml", Pod: "train-a"},
			{PID: 200, GpuUUID: "u0", Type: "graphics", MemoryUsedBytes: 2048},
		},
	})
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes?node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuProcessesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Available || resp.Reason != "" || resp.CollectedAt != "2026-07-13T00:00:00Z" {
		t.Errorf("resp=%+v want available/collected_at 중계", resp)
	}
	if len(resp.Processes) != 2 {
		t.Fatalf("processes=%d want 2: %+v", len(resp.Processes), resp.Processes)
	}
	p := resp.Processes[0]
	if p.PID != 100 || p.Pod != "train-a" || p.SmUtilPercent == nil || *p.SmUtilPercent != 30 {
		t.Errorf("p=%+v want 100/train-a/sm 30", p)
	}
	if !strings.Contains(resp.Summary, "2개") || !strings.Contains(resp.Summary, "귀속 1") {
		t.Errorf("summary=%q want 프로세스 2개 / pod 귀속 1", resp.Summary)
	}
}

// TestGpuProcesses_NoAgent 는 up 시리즈 부재 (gpuobs 미배치 노드) 시 available=false 와 사유로
// graceful 응답하는지 검증한다.
func TestGpuProcesses_NoAgent(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes?node=worker1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (graceful)", rec.Code)
	}
	var resp GpuProcessesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Available || resp.Reason == "" || len(resp.Processes) != 0 {
		t.Errorf("resp=%+v want available=false / reason / 빈 목록", resp)
	}
}

// TestGpuProcesses_AgentDown 은 up=0 (스크레이프 실패) 시 dial 없이 graceful 응답하는지 검증한다.
func TestGpuProcesses_AgentDown(t *testing.T) {
	q := (&fakeQuerier{}).on(`up{job="gpuobs-agent"`, sample(0, "node", "gpu", "instance", "10.0.0.1:9820"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes?node=gpu", nil))
	var resp GpuProcessesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Available || !strings.Contains(resp.Reason, "down") {
		t.Errorf("resp=%+v want down 사유", resp)
	}
}

// TestGpuProcesses_InvalidInstance 는 instance 라벨이 IP:port 형식이 아니면 dial 하지 않고 사유로
// 응답하는지 검증한다 (임의 host 호출 표면 차단).
func TestGpuProcesses_InvalidInstance(t *testing.T) {
	q := (&fakeQuerier{}).on(`up{job="gpuobs-agent"`, sample(1, "node", "gpu", "instance", "evil.example.com:9820"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes?node=gpu", nil))
	var resp GpuProcessesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Available || !strings.Contains(resp.Reason, "유효하지 않습니다") {
		t.Errorf("resp=%+v want instance 형식 사유", resp)
	}
}

// TestGpuProcesses_AgentUnreachable 은 agent 호출 실패 (connection refused) 시 graceful 응답을
// 검증한다.
func TestGpuProcesses_AgentUnreachable(t *testing.T) {
	srv, q := gpuProcessesAgent(t, gpuobstypes.GPUProcessListing{})
	srv.Close()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes?node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (graceful)", rec.Code)
	}
	var resp GpuProcessesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Available || !strings.Contains(resp.Reason, "agent 응답 실패") {
		t.Errorf("resp=%+v want 응답 실패 사유", resp)
	}
}

// TestGpuProcesses_MissingNode 는 node 누락 400, 형식 위반 400 (쿼리 실행 전 차단) 을 검증한다.
func TestGpuProcesses_MissingNode(t *testing.T) {
	q := &fakeQuerier{}
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("누락 status=%d want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	target := "/api/v1/gpu-processes?node=" + url.QueryEscape(`gpu"} or up{`)
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("형식 위반 status=%d want 400", rec.Code)
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v", q.queries)
	}
}

// TestGpuProcesses_QueryError 는 up 조회 실패 (Prometheus 인프라 문제) 가 graceful 이 아닌 500 으로
// 구분되는지 검증한다.
func TestGpuProcesses_QueryError(t *testing.T) {
	h := NewSynthesisHandler(errQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes?node=gpu", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// TestGpuProcesses_NilQuerier 는 querier 미주입 시 graceful 빈 응답을 검증한다.
func TestGpuProcesses_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuProcesses(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-processes?node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuProcessesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Available || len(resp.Processes) != 0 {
		t.Errorf("resp=%+v want 빈 graceful 응답", resp)
	}
}

// TestValidAgentInstance_Hardening 은 #409 의 사설 대역과 포트 allow-list 검사를 고정한다.
func TestValidAgentInstance_Hardening(t *testing.T) {
	valid := []string{"172.16.0.5:9820", "10.0.0.1:9810", "192.168.1.9:9850", "127.0.0.1:9830"}
	for _, in := range valid {
		if !validAgentInstance(in) {
			t.Errorf("validAgentInstance(%q)=false want true", in)
		}
	}
	invalid := []string{
		"8.8.8.8:9820",          // 공인 대역
		"169.254.169.254:9820",  // link-local (메타데이터 endpoint)
		"172.16.0.5:22",         // 비 agent 포트
		"172.16.0.5:80",         // 비 agent 포트
		"evil.example.com:9820", // hostname
		"172.16.0.5",            // 포트 없음
	}
	for _, in := range invalid {
		if validAgentInstance(in) {
			t.Errorf("validAgentInstance(%q)=true want false", in)
		}
	}
}
