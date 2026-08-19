package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
		Webhook:  NewWebhookHandler(rcaReg, nil, st, met, 0.0),
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
	_ = resp.Body.Close()
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
	defer func() { _ = resp.Body.Close() }()
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
	_ = resp.Body.Close()
	if _, ok := st.Get("NetObsDropBurst"); ok {
		t.Errorf("resolved alert should not have been stored")
	}
}

// TestWebhook_EmittedCounterIncrements 는 emit 카운터가 alert 발화마다 1 씩 증가하는지 검증한다.
// #419 부터 동일 labels 의 짧은 창 내 재전송은 멱등 억제되므로 발화마다 라벨 (src_pod) 을 다르게
// 둬 서로 다른 alert 3건임을 명시한다.
func TestWebhook_EmittedCounterIncrements(t *testing.T) {
	mux, _, met, _ := fixtures(t)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for i := 0; i < 3; i++ {
		payload := fmt.Sprintf(`{"alerts":[{"status":"firing","labels":{"alertname":"GPUIdleWithCPUThrottle","src_namespace":"perf","src_pod":"p%d","node":"gpu"}}]}`, i)
		resp, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(payload))
		_ = resp.Body.Close()
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
	_ = r1.Body.Close()

	// 두 번째 발화 (다른 dst_port)
	p2 := `{"alerts":[{"status":"firing","labels":{
		"alertname":"NetObsDropBurst","src_namespace":"default","src_pod":"c1",
		"dst_ip":"10.0.0.5","dst_port":"9090","protocol":"tcp","drop_reason":"TCP_RESET"
	}}]}`
	r2, _ := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(p2))
	_ = r2.Body.Close()

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
	_ = r.Body.Close()

	resp, err := http.Get(srv.URL + "/rca")
	if err != nil {
		t.Fatalf("GET /rca: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
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
	_ = resp.Body.Close()
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
	_ = r.Body.Close()
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
	_ = r.Body.Close()

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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status=%d; want non-200 (oversized payload must be rejected)", resp.StatusCode)
	}
}

// ctxCapturingSources 는 #419 컨텍스트 전파 검증용 fake 다. Dispatch 경유로 받은 ctx 를 보관해
// 테스트가 요청 컨텍스트와의 연결 (취소 전파) 을 단정할 수 있게 한다.
type ctxCapturingSources struct {
	mu   sync.Mutex
	ctxs []context.Context
}

func (f *ctxCapturingSources) TopNeighbors(ctx context.Context, _, _ string) []registry.NeighborInfo {
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	return nil
}
func (f *ctxCapturingSources) TopDropFlows(ctx context.Context, _ string) []registry.DropFlowInfo {
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	return nil
}
func (f *ctxCapturingSources) GPUSignal(ctx context.Context, _ string) float64 {
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	return 0
}
func (f *ctxCapturingSources) EvaluateConfidence([]registry.NeighborInfo, []registry.DropFlowInfo, float64) float64 {
	return 1
}

// TestWebhook_RequestContextPropagatesToSources 는 #419 의 취소 전파를 단정한다. 요청 컨텍스트를
// 취소하면 하류 source 가 받은 ctx 도 즉시 Done 이어야 한다.
func TestWebhook_RequestContextPropagatesToSources(t *testing.T) {
	src := &ctxCapturingSources{}
	st := store.New()
	met := rcametrics.New()
	h := NewWebhookHandler(registry.New(), src, st, met, 0.0)

	body := `{"alerts":[{"status":"firing","labels":{"alertname":"NetObsDropBurst","src_namespace":"ns1","src_pod":"p1"}}]}`
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	src.mu.Lock()
	captured := append([]context.Context(nil), src.ctxs...)
	src.mu.Unlock()
	if len(captured) == 0 {
		t.Fatal("source 가 호출되지 않음 (mapping 이 source 를 타지 않음)")
	}
	select {
	case <-captured[0].Done():
		t.Fatal("요청 취소 전에 ctx 가 이미 Done")
	default:
	}
	cancel()
	select {
	case <-captured[0].Done():
	default:
		t.Fatal("요청 컨텍스트 취소가 source ctx 로 전파되지 않음")
	}
}

// countingSources 는 하류 호출 수와 동시 실행 수를 계수하는 fake 다 (#419).
type countingSources struct {
	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
	block       time.Duration
}

func (f *countingSources) enter() {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	f.mu.Unlock()
	if f.block > 0 {
		time.Sleep(f.block)
	}
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}
func (f *countingSources) TopNeighbors(_ context.Context, _, _ string) []registry.NeighborInfo {
	f.enter()
	return nil
}
func (f *countingSources) TopDropFlows(_ context.Context, _ string) []registry.DropFlowInfo {
	f.enter()
	return nil
}
func (f *countingSources) GPUSignal(_ context.Context, _ string) float64 {
	f.enter()
	return 0
}
func (f *countingSources) EvaluateConfidence([]registry.NeighborInfo, []registry.DropFlowInfo, float64) float64 {
	return 1
}

// webhookFixture 는 counting fake 를 물린 webhook 핸들러를 만든다.
func webhookFixture(src registry.Sources, met *rcametrics.Metrics) http.Handler {
	return NewWebhookHandler(registry.New(), src, store.New(), met, 0.0)
}

// bulkPayload 는 서로 다른 라벨의 firing alert n건 payload 를 만든다.
func bulkPayload(n int) string {
	var b strings.Builder
	b.WriteString(`{"alerts":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"status":"firing","labels":{"alertname":"NetObsDropBurst","src_namespace":"ns1","src_pod":"p%d"}}`, i)
	}
	b.WriteString("]}")
	return b.String()
}

// TestWebhook_AlertCapBoundsDownstreamCalls 는 #419 의 상한을 단정한다. 상한 초과 payload 에서
// dispatch 가 MaxAlertsPerWebhook 건으로 잘리고 초과분이 over_cap 으로 계수되며, 하류 호출 수가
// 상한 x alert 당 최대 3회 이하로 유계임을 함께 확인한다.
func TestWebhook_AlertCapBoundsDownstreamCalls(t *testing.T) {
	src := &countingSources{}
	met := rcametrics.New()
	h := webhookFixture(src, met)

	n := MaxAlertsPerWebhook + 16
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(bulkPayload(n))))
	elapsed := time.Since(start)

	var resp struct {
		Processed int `json:"processed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response decode: %v", err)
	}
	if resp.Processed != MaxAlertsPerWebhook {
		t.Errorf("processed=%d want %d", resp.Processed, MaxAlertsPerWebhook)
	}
	if src.calls > MaxAlertsPerWebhook*3 {
		t.Errorf("downstream calls=%d exceeds cap x 3 = %d", src.calls, MaxAlertsPerWebhook*3)
	}
	if elapsed > 5*time.Second {
		t.Errorf("processing took %v; want well under timeout budget", elapsed)
	}
}

// TestWebhook_DispatchConcurrencyBounded 는 유계 동시성을 단정한다. blocking fake 로 동시 실행
// 최대치가 webhookDispatchWorkers 이하임을 확인한다.
func TestWebhook_DispatchConcurrencyBounded(t *testing.T) {
	src := &countingSources{block: 30 * time.Millisecond}
	met := rcametrics.New()
	h := webhookFixture(src, met)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(bulkPayload(16))))

	if src.maxInFlight > webhookDispatchWorkers {
		t.Errorf("max in-flight=%d exceeds workers=%d", src.maxInFlight, webhookDispatchWorkers)
	}
	if src.maxInFlight < 2 {
		t.Errorf("max in-flight=%d; 직렬 처리로 보임 (동시성 미동작)", src.maxInFlight)
	}
}

// TestWebhook_RetransmissionDeduped 는 멱등 억제를 단정한다. 동일 payload 재전송은 하류 호출
// 없이 duplicate 로 계수되고, 라벨이 다른 alert 는 억제되지 않는다.
func TestWebhook_RetransmissionDeduped(t *testing.T) {
	src := &countingSources{}
	met := rcametrics.New()
	h := webhookFixture(src, met)

	payload := `{"alerts":[{"status":"firing","labels":{"alertname":"NetObsDropBurst","src_namespace":"ns1","src_pod":"p1"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload)))
	callsAfterFirst := src.calls

	// 재전송 (동일 labels): 하류 호출이 늘지 않아야 한다.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(payload)))
	if src.calls != callsAfterFirst {
		t.Errorf("재전송이 하류 호출을 유발: calls %d -> %d", callsAfterFirst, src.calls)
	}
	var resp struct {
		Processed int `json:"processed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Processed != 0 {
		t.Errorf("재전송 processed=%d want 0", resp.Processed)
	}

	// 다른 라벨: 억제되지 않는다.
	other := `{"alerts":[{"status":"firing","labels":{"alertname":"NetObsDropBurst","src_namespace":"ns1","src_pod":"p2"}}]}`
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(other)))
	if src.calls <= callsAfterFirst {
		t.Errorf("다른 라벨 alert 가 억제됨: calls=%d", src.calls)
	}
}

// TestWebhook_StoreCapRejectionObserved는 #446의 store 거부 관측성을 검증한다. 종전에는 st.Set의
// 반환값을 버려 미등록 alert의 cap 초과 드롭이 완전히 무관측이었다. cap 2 store를 미등록 alert
// 2종으로 채운 뒤 세 번째 미등록 alert이 거부되면 rca_store_entries_rejected_total이 계수되고
// /rca 조회가 404 임을 단정한다.
func TestWebhook_StoreCapRejectionObserved(t *testing.T) {
	reg := prometheus.NewRegistry()
	st := store.NewWithMaxEntries(2)
	met := rcametrics.New()
	for _, c := range met.Collectors() {
		reg.MustRegister(c)
	}
	var ready atomic.Bool
	ready.Store(true)
	mux := NewMux(Options{
		Registry: reg,
		Ready:    &ready,
		Webhook:  NewWebhookHandler(registry.New(), nil, st, met, 0.0),
		RCA:      NewRCAHandler(st),
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := func(alertname string) {
		t.Helper()
		payload := `{"alerts":[{"status":"firing","labels":{"alertname":"` + alertname + `"}}]}`
		resp, err := http.Post(srv.URL+"/webhook", "application/json", strings.NewReader(payload))
		if err != nil {
			t.Fatalf("POST %s: %v", alertname, err)
		}
		_ = resp.Body.Close()
	}
	post("UnregisteredA")
	post("UnregisteredB")
	post("UnregisteredC")

	if st.Len() != 2 {
		t.Fatalf("store len=%d want 2 (cap)", st.Len())
	}
	if _, ok := st.Get("UnregisteredC"); ok {
		t.Fatalf("UnregisteredC 가 cap 초과인데 저장됨")
	}
	body, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = body.Body.Close() }()
	raw, _ := io.ReadAll(body.Body)
	if !strings.Contains(string(raw), "rca_store_entries_rejected_total 1") {
		t.Errorf("rejected counter 미계수: %s", grepLine(string(raw), "rca_store_entries_rejected_total"))
	}
}

// grepLine은 메트릭 텍스트에서 해당 이름이 포함된 행을 뽑는 테스트 헬퍼다.
func grepLine(body, name string) string {
	for _, l := range strings.Split(body, "\n") {
		if strings.Contains(l, name) && !strings.HasPrefix(l, "#") {
			return l
		}
	}
	return "(없음)"
}
