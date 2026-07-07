package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"netobs/internal/correlation"
)

// errQuerier 는 항상 에러를 돌려주는 InstantQuerier 더블이다.
type errQuerier struct{}

func (errQuerier) Query(context.Context, string) ([]correlation.InstantSample, error) {
	return nil, errors.New("prometheus unreachable")
}

// TestLatencyBreakdown_Workload 는 scope=workload 가 (workload, stage) p99 를 대상별로 묶어 단계 분해와
// 지배 단계, share 를 만들고, 대상을 worst stage p99 내림차순으로 정렬하는지 검증한다.
func TestLatencyBreakdown_Workload(t *testing.T) {
	q := (&fakeQuerier{}).on("netobs_stage_latency_labeled_seconds_bucket",
		sample(0.001, "src_namespace", "default", "src_workload", "app", "stage", "sendmsg_ret"),
		sample(0.005, "src_namespace", "default", "src_workload", "app", "stage", "to_veth"),
		sample(0.002, "src_namespace", "default", "src_workload", "other", "stage", "rcv_app"))

	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetLatencyBreakdown(rec, httptest.NewRequest(http.MethodGet, "/api/v1/latency-breakdown", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp LatencyBreakdownResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Scope != "workload" || len(resp.Targets) != 2 {
		t.Fatalf("scope=%q targets=%d want workload/2", resp.Scope, len(resp.Targets))
	}
	// app(worst 0.005) 이 other(worst 0.002) 보다 먼저.
	app := resp.Targets[0]
	if app.Workload != "app" || app.Namespace != "default" || app.DominantStage != "to_veth" {
		t.Errorf("target[0]=%+v want default/app dominant to_veth", app)
	}
	if len(app.Stages) != 2 || app.Stages[0].Stage != "to_veth" || app.Stages[0].Share <= 0.8 {
		t.Errorf("app stages=%+v want to_veth 먼저 share>0.8", app.Stages)
	}
	if resp.Targets[1].Workload != "other" {
		t.Errorf("target[1]=%+v want other", resp.Targets[1])
	}
}

// TestLatencyBreakdown_PodTcpState 는 #226 의 pod scope 한정 TCP 상태 join 을 검증한다. srtt 와 cwnd
// 가 (namespace, pod) 키로 붙고, tcp_state 시리즈가 없는 pod 는 필드가 생략된다.
func TestLatencyBreakdown_PodTcpState(t *testing.T) {
	q := (&fakeQuerier{}).
		on("netobs_pod_stage_latency_labeled_seconds_bucket",
			sample(0.003, "src_namespace", "default", "src_pod", "p1", "stage", "sendmsg_ret"),
			sample(0.001, "src_namespace", "default", "src_pod", "p2", "stage", "sendmsg_ret")).
		on("netobs_tcp_state_max_srtt_seconds",
			sample(0.036, "namespace", "default", "pod", "p1")).
		on("netobs_tcp_state_min_cwnd",
			sample(10, "namespace", "default", "pod", "p1"))

	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetLatencyBreakdown(rec, httptest.NewRequest(http.MethodGet, "/api/v1/latency-breakdown?scope=pod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp LatencyBreakdownResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Targets) != 2 {
		t.Fatalf("targets=%d want 2", len(resp.Targets))
	}
	p1 := resp.Targets[0]
	if p1.Pod != "p1" || p1.TcpState == nil || p1.TcpState.MaxSrttSeconds == nil || *p1.TcpState.MaxSrttSeconds != 0.036 {
		t.Errorf("p1 tcp_state=%+v want srtt 0.036 join", p1.TcpState)
	}
	if p1.TcpState != nil && (p1.TcpState.MinCwnd == nil || *p1.TcpState.MinCwnd != 10) {
		t.Errorf("p1 min_cwnd=%+v want 10", p1.TcpState)
	}
	if resp.Targets[1].TcpState != nil {
		t.Errorf("p2 tcp_state=%+v want nil (시리즈 없음)", resp.Targets[1].TcpState)
	}
}

// TestLatencyBreakdown_InvalidScope 는 알 수 없는 scope 에 400 을 돌려주는지 검증한다.
func TestLatencyBreakdown_InvalidScope(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetLatencyBreakdown(rec, httptest.NewRequest(http.MethodGet, "/api/v1/latency-breakdown?scope=disk", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid scope)", rec.Code)
	}
}

// TestLatencyBreakdown_InvalidDirection 은 허용되지 않은 direction 에 400 을 돌려주는지 검증한다 (PromQL
// injection 방지용 리터럴 화이트리스트).
func TestLatencyBreakdown_InvalidDirection(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetLatencyBreakdown(rec, httptest.NewRequest(http.MethodGet, `/api/v1/latency-breakdown?direction=egress"}or(`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid direction)", rec.Code)
	}
}

// TestLatencyBreakdown_QueryError 는 Prometheus 쿼리 실패 시 빈 200 이 아니라 500 을 돌려주는지 검증한다.
func TestLatencyBreakdown_QueryError(t *testing.T) {
	h := NewSynthesisHandler(errQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetLatencyBreakdown(rec, httptest.NewRequest(http.MethodGet, "/api/v1/latency-breakdown", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (query 실패)", rec.Code)
	}
}

// TestLatencyBreakdown_NilQuerier 는 querier 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestLatencyBreakdown_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetLatencyBreakdown(rec, httptest.NewRequest(http.MethodGet, "/api/v1/latency-breakdown", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp LatencyBreakdownResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Targets) != 0 {
		t.Errorf("targets=%d want 0 (nil querier)", len(resp.Targets))
	}
}
