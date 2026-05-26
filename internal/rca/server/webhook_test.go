package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	rcametrics "netobs/internal/rca/metrics"
	"netobs/internal/rca/registry"
	"netobs/internal/rca/store"
)

// fixtures 는 webhook 핸들러의 통합 흐름 (parse → dispatch → store → metrics → /rca) 을 한 곳에서
// 구성한다.
func fixtures(t *testing.T) (http.Handler, *store.Store, *rcametrics.Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	st := store.New()
	met := rcametrics.New()
	for _, c := range met.Collectors() {
		reg.MustRegister(c)
	}
	rcaReg := registry.New()
	var ready atomic.Bool
	ready.Store(true)
	mux := NewMux(Options{
		Registry: reg,
		Ready:    &ready,
		Webhook:  NewWebhookHandler(rcaReg, nil, st, met),
		RCA:      NewRCAHandler(st),
	})
	return mux, st, met, reg
}

// TestWebhook_NetObsDropBurstPersistsSummary 는 5-tuple 라벨 셋의 payload 가 store 에 저장되고
// /rca?alert=... 로 동일 RCASummary 가 즉시 응답되는지 검증한다.
func TestWebhook_NetObsDropBurstPersistsSummary(t *testing.T) {
	mux, st, _, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	payload := `{"alerts":[{"status":"firing","labels":{
		"alertname":"NetObsDropBurst",
		"src_namespace":"default","src_pod":"client",
		"dst_ip":"10.0.0.5","dst_port":"8080",
		"protocol":"tcp","drop_reason":"TCP_RESET"
	}}]}`
	resp, err := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d; want 200", resp.StatusCode)
	}

	entry, ok := st.Get("NetObsDropBurst")
	if !ok {
		t.Fatalf("store missing NetObsDropBurst")
	}
	if entry.Summary.DominantDimension != "network" {
		t.Errorf("dim=%q", entry.Summary.DominantDimension)
	}
	if !strings.Contains(entry.Summary.PrimaryDropFlow, "10.0.0.5:8080") {
		t.Errorf("primary=%q", entry.Summary.PrimaryDropFlow)
	}

	// /rca endpoint 응답 검증
	resp, err = http.Get(srv.URL + "/rca?alert=NetObsDropBurst")
	if err != nil {
		t.Fatalf("GET /rca: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/rca status=%d", resp.StatusCode)
	}
	var got store.Entry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Summary.AlertName != "NetObsDropBurst" {
		t.Errorf("got=%+v", got.Summary)
	}
}

// TestWebhook_ResolvedAlertSkipped 는 status=resolved 인 알람이 store 에 저장되지 않는지 검증한다.
func TestWebhook_ResolvedAlertSkipped(t *testing.T) {
	mux, st, _, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	payload := `{"alerts":[{"status":"resolved","labels":{"alertname":"NetObsDropBurst"}}]}`
	resp, err := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if _, ok := st.Get("NetObsDropBurst"); ok {
		t.Errorf("resolved alert should not have been stored")
	}
}

// TestWebhook_EmittedCounterIncrements 는 emit 카운터가 alert 발화마다 1 씩 증가하는지 검증한다.
func TestWebhook_EmittedCounterIncrements(t *testing.T) {
	mux, _, met, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	payload := `{"alerts":[{"status":"firing","labels":{"alertname":"GPUIdleWithCPUThrottle","src_namespace":"perf","src_pod":"p","node":"gpu"}}]}`
	for i := 0; i < 3; i++ {
		resp, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(payload))
		resp.Body.Close()
	}

	// emit_total 은 alert당 누적이므로 3 이어야 한다
	collectors := met.Collectors()
	cv, ok := collectors[0].(*prometheus.CounterVec)
	if !ok {
		t.Fatalf("collectors[0] not CounterVec")
	}
	got := testutil.ToFloat64(cv.WithLabelValues("GPUIdleWithCPUThrottle"))
	if got != 3 {
		t.Errorf("emitted_total=%v; want 3", got)
	}
}

// TestWebhook_LastSummaryInfoCardinalityCollapsed 는 같은 alert 가 다른 라벨 셋으로 다시 발화해도
// last_summary_info 시리즈가 alert 당 1 개로 유지되는지 검증한다 (이전 라벨 셋 자동 delete).
func TestWebhook_LastSummaryInfoCardinalityCollapsed(t *testing.T) {
	mux, _, met, reg := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 첫 발화
	p1 := `{"alerts":[{"status":"firing","labels":{
		"alertname":"NetObsDropBurst","src_namespace":"default","src_pod":"c1",
		"dst_ip":"10.0.0.5","dst_port":"8080","protocol":"tcp","drop_reason":"TCP_RESET"
	}}]}`
	r1, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(p1))
	r1.Body.Close()

	// 두 번째 발화 (다른 dst_port)
	p2 := `{"alerts":[{"status":"firing","labels":{
		"alertname":"NetObsDropBurst","src_namespace":"default","src_pod":"c1",
		"dst_ip":"10.0.0.5","dst_port":"9090","protocol":"tcp","drop_reason":"TCP_RESET"
	}}]}`
	r2, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(p2))
	r2.Body.Close()

	// last_summary_info 의 NetObsDropBurst 시리즈 수 == 1
	gv, ok := met.Collectors()[1].(*prometheus.GaugeVec)
	if !ok {
		t.Fatalf("collectors[1] not GaugeVec")
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var seriesCount int
	for _, mf := range mfs {
		if mf.GetName() != "rca_summary_last_summary_info" {
			continue
		}
		for _, m := range mf.Metric {
			for _, lp := range m.Label {
				if lp.GetName() == "alert_name" && lp.GetValue() == "NetObsDropBurst" {
					seriesCount++
				}
			}
		}
	}
	if seriesCount != 1 {
		t.Errorf("NetObsDropBurst series=%d; want 1 (cardinality must collapse)", seriesCount)
	}
	_ = gv
}

// TestRCAHandler_AllReturnsArray 는 alert query 가 비어 있을 때 store 전체 entry 가 JSON 배열로
// 응답되는지 검증한다.
func TestRCAHandler_AllReturnsArray(t *testing.T) {
	mux, _, _, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := `{"alerts":[{"status":"firing","labels":{"alertname":"GPUIdleWithCPUThrottle","src_namespace":"perf","src_pod":"p"}}]}`
	r, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(p))
	r.Body.Close()

	resp, err := http.Get(srv.URL + "/rca")
	if err != nil {
		t.Fatalf("GET /rca: %v", err)
	}
	defer resp.Body.Close()
	var got []store.Entry
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("len=%d; want 1", len(got))
	}
}

// TestRCAHandler_UnknownAlertReturns404 는 store 에 없는 alert 조회 시 404 를 돌려주는지 검증한다.
func TestRCAHandler_UnknownAlertReturns404(t *testing.T) {
	mux, _, _, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/rca?alert=NoSuchAlert")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d; want 404", resp.StatusCode)
	}
}

// TestWebhook_UnknownAlertEchoesRawLabels 는 mapping 미등록 alert 가 silent drop 되지 않고
// store 에 raw label echo 형태의 entry 가 저장되는지 검증한다.
func TestWebhook_UnknownAlertEchoesRawLabels(t *testing.T) {
	mux, st, _, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := `{"alerts":[{"status":"firing","labels":{"alertname":"NotMapped","foo":"bar"}}]}`
	r, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(p))
	r.Body.Close()
	if _, ok := st.Get("NotMapped"); !ok {
		t.Errorf("unmapped alert should still be stored for diagnostic")
	}
}

// TestWebhook_UnknownAlertSkipsMetricsEmit 는 mapping 미등록 alert 가 store 에는 저장되지만
// metrics emit 은 건너뛰어 rca_summary_emitted_total 의 alert_name 라벨이 9 종으로 폐쇄되는지
// 검증한다. 외부에서 임의 alertname 으로 webhook 이 도달해도 cardinality 가 폭증하지 않는다.
func TestWebhook_UnknownAlertSkipsMetricsEmit(t *testing.T) {
	mux, _, met, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := `{"alerts":[{"status":"firing","labels":{"alertname":"AdversarialNoise","foo":"bar"}}]}`
	r, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(p))
	r.Body.Close()

	cv, ok := met.Collectors()[0].(*prometheus.CounterVec)
	if !ok {
		t.Fatalf("collectors[0] not CounterVec")
	}
	if got := testutil.ToFloat64(cv.WithLabelValues("AdversarialNoise")); got != 0 {
		t.Errorf("emitted_total{AdversarialNoise}=%v; want 0 (unmapped alert must not emit metrics)", got)
	}
}

// TestWebhook_OversizedPayloadRejected 는 MaxWebhookPayloadBytes 를 넘는 본문이 400 응답으로
// 차단되는지 검증한다. http.MaxBytesReader 가 한도 초과 시 ReadAll 단계에서 에러를 돌려준다.
func TestWebhook_OversizedPayloadRejected(t *testing.T) {
	mux, _, _, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 한도 (1 MiB) 보다 큰 dummy payload. 정상적이지 않은 JSON 이라도 ReadAll 단계에서 차단된다.
	huge := strings.Repeat("a", (1<<20)+1024)
	resp, err := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status=%d; want non-200 (oversized payload must be rejected)", resp.StatusCode)
	}
}
