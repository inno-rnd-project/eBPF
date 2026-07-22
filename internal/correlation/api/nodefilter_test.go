package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestNodeFilter_ExactMatcher 는 #263 의 node 필터가 다섯 핸들러에서 exact `=` 매처로 PromQL 에
// 결합되고 `=~` 정규식 매처를 쓰지 않는지 검증한다. 각 핸들러가 node 를 받은 쿼리를 최소 하나
// 실행했는지 sawQuery 로 확인한다.
func TestNodeFilter_ExactMatcher(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		invoke  func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request)
		wantSub string
	}{
		{
			"pressure", "/api/v1/pressure?dimension=cpu&node=gpu",
			func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetPressure(rec, req) },
			`node="gpu"`,
		},
		{
			"gpu-status", "/api/v1/gpu-status?node=gpu",
			func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetGpuStatus(rec, req) },
			`gpuobs_device_utilization_percent{node="gpu"}`,
		},
		{
			"drops", "/api/v1/drops?node=gpu",
			func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetDrops(rec, req) },
			`netobs_drop_events_labeled_total{node="gpu"}`,
		},
		{
			"bandwidth", "/api/v1/bandwidth?node=gpu",
			func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetBandwidth(rec, req) },
			`netobs_pod_bytes_total{node="gpu"}`,
		},
		{
			"latency-breakdown", "/api/v1/latency-breakdown?scope=node&node=gpu",
			func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) {
				h.GetLatencyBreakdown(rec, req)
			},
			`node="gpu"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{}
			h := NewSynthesisHandler(q, nil, nil)
			rec := httptest.NewRecorder()
			tc.invoke(h, rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s status=%d want 200", tc.name, rec.Code)
			}
			if !q.sawQuery(tc.wantSub) {
				t.Errorf("%s: exact 매처 %q 결합 쿼리 미확인: %v", tc.name, tc.wantSub, q.queries)
			}
			if q.sawQuery("=~") {
				t.Errorf("%s: 정규식 매처(=~)가 쓰임: %v", tc.name, q.queries)
			}
		})
	}
}

// TestNodeFilter_InvalidNode 는 다섯 핸들러 모두 DNS-1123 위반 node 를 PromQL 결합 전에 400 으로
// 거부하고 쿼리를 실행하지 않는지 검증한다.
func TestNodeFilter_InvalidNode(t *testing.T) {
	bad := url.QueryEscape(`gpu"} or up{`)
	cases := []struct {
		name   string
		target string
		invoke func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request)
	}{
		{"pressure", "/api/v1/pressure?dimension=cpu&node=" + bad, func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetPressure(rec, req) }},
		{"gpu-status", "/api/v1/gpu-status?node=" + bad, func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetGpuStatus(rec, req) }},
		{"drops", "/api/v1/drops?node=" + bad, func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetDrops(rec, req) }},
		{"bandwidth", "/api/v1/bandwidth?node=" + bad, func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetBandwidth(rec, req) }},
		{"latency-breakdown", "/api/v1/latency-breakdown?node=" + bad, func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.GetLatencyBreakdown(rec, req)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{}
			h := NewSynthesisHandler(q, nil, nil)
			rec := httptest.NewRecorder()
			tc.invoke(h, rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s status=%d want 400", tc.name, rec.Code)
			}
			if len(q.queries) != 0 {
				t.Errorf("%s: 거부 후 쿼리 실행됨: %v", tc.name, q.queries)
			}
		})
	}
}
