package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"netobs/internal/correlation"
)

// fakeQuerier 는 query 문자열별 canned InstantSample 을 돌려주는 테스트 더블이다. 부분 문자열 매칭으로
// topk wrapping 이나 node 필터가 붙은 query 도 같은 규칙에 걸리게 한다.
type fakeQuerier struct {
	rules []struct {
		contains string
		samples  []correlation.InstantSample
	}
	mu        sync.Mutex
	lastQuery string
	queries   []string
	lastAt    time.Time
}

// sawQuery 는 실행된 쿼리 중 sub 를 포함하는 것이 있는지 돌려준다. queryParallel 이 Query 를 동시
// 호출하므로 mutex 로 보호한다.
func (f *fakeQuerier) sawQuery(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.queries {
		if strings.Contains(q, sub) {
			return true
		}
	}
	return false
}

func (f *fakeQuerier) on(contains string, samples ...correlation.InstantSample) *fakeQuerier {
	f.rules = append(f.rules, struct {
		contains string
		samples  []correlation.InstantSample
	}{contains, samples})
	return f
}

func (f *fakeQuerier) Query(ctx context.Context, query string) ([]correlation.InstantSample, error) {
	f.mu.Lock()
	f.lastQuery = query
	f.queries = append(f.queries, query)
	// #235 시점 지정 조회 검증용. ctx 에 실린 평가 시점을 기록한다.
	if t, ok := correlation.QueryTimeFrom(ctx); ok {
		f.lastAt = t
	}
	f.mu.Unlock()
	for _, r := range f.rules {
		if strings.Contains(query, r.contains) {
			return r.samples, nil
		}
	}
	return nil, nil
}

func sample(v float64, kv ...string) correlation.InstantSample {
	l := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return correlation.InstantSample{Labels: l, Value: v}
}

// TestSynthesis_GetHealth 는 /api/v1/health 가 차원별 health·status·hotspot·dominant·anomaly·summary
// 를 합성하는지 검증한다. cpu 가 degraded 이고 worker2/app-x 에 압박이 집중되며 dominant 로 채택되는
// 시나리오다.
func TestSynthesis_GetHealth(t *testing.T) {
	q := (&fakeQuerier{}).
		on("cluster:cpu_health_score", sample(0.45)).
		on("cluster:gpu_health_score", sample(0.82)).
		on("cluster:memory_health_score", sample(0.90)).
		on("cluster:network_health_score", sample(0.60)).
		on("node:cpu_pressure_score", sample(0.78, "node", "worker2")).
		on("pod:cpu_throttle_score", sample(0.78, "src_namespace", "default", "src_pod", "app-x")).
		on("cluster:cpu_throttle_zscore", sample(3.2))
	// gpu/memory/network 의 node pressure 는 미설정 → hotspot nil (graceful).

	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetHealth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	cpu := resp.Dimensions["cpu"]
	if cpu.Status != "degraded" || cpu.Health == nil || *cpu.Health != 0.45 {
		t.Errorf("cpu=%+v want degraded/0.45", cpu)
	}
	if cpu.Hotspot == nil || cpu.Hotspot.Node != "worker2" || cpu.Hotspot.TopPod != "default/app-x" || cpu.Hotspot.Severity != "high" {
		t.Errorf("cpu hotspot=%+v want worker2/default/app-x/high", cpu.Hotspot)
	}
	if resp.Dimensions["gpu"].Status != "ok" {
		t.Errorf("gpu status=%q want ok", resp.Dimensions["gpu"].Status)
	}
	if resp.DominantPressure == nil || resp.DominantPressure.Dimension != "cpu" || resp.DominantPressure.Pod != "default/app-x" {
		t.Errorf("dominant=%+v want cpu/default/app-x", resp.DominantPressure)
	}
	if len(resp.Anomalies) != 1 || resp.Anomalies[0].Dimension != "cpu" || resp.Anomalies[0].Severity != "high" {
		t.Errorf("anomalies=%+v want 1 cpu/high", resp.Anomalies)
	}
	if !strings.Contains(resp.Summary, "cpu") {
		t.Errorf("summary=%q want cpu 언급", resp.Summary)
	}
	// #248 가장 약한 고리: 최저 health 차원 (cpu 0.45) 이 cluster_health 와 weakest 로 승격된다.
	if resp.ClusterHealth == nil || *resp.ClusterHealth != 0.45 {
		t.Errorf("cluster_health=%v want 0.45", resp.ClusterHealth)
	}
	if resp.Weakest == nil || resp.Weakest.Dimension != "cpu" || resp.Weakest.Health != 0.45 || resp.Weakest.Status != "degraded" {
		t.Errorf("weakest=%+v want cpu/0.45/degraded", resp.Weakest)
	}
}

// TestSynthesis_GetPressure 는 dimension=cpu&scope=pod 가 pressure 내림차순 랭킹을 rank·severity 와
// 함께 돌려주는지 검증한다.
func TestSynthesis_GetPressure(t *testing.T) {
	q := (&fakeQuerier{}).on("pod:cpu_throttle_score",
		sample(0.51, "node", "worker2", "src_namespace", "batch", "src_pod", "job-7"),
		sample(0.78, "node", "worker2", "src_namespace", "default", "src_pod", "app-x"),
	)
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=cpu&scope=pod&limit=10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp PressureResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Ranking) != 2 || resp.Ranking[0].Pod != "default/app-x" || resp.Ranking[0].Rank != 1 || resp.Ranking[0].Severity != "high" {
		t.Errorf("ranking=%+v want app-x rank1 high 먼저", resp.Ranking)
	}
	if resp.Ranking[1].Pressure != 0.51 {
		t.Errorf("rank2 pressure=%v want 0.51 (내림차순)", resp.Ranking[1].Pressure)
	}
}

// TestSynthesis_GetPressure_InvalidDimension 은 알 수 없는 dimension 에 400 을 돌려주는지 검증한다.
func TestSynthesis_GetPressure_InvalidDimension(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=disk", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid dimension)", rec.Code)
	}
}

// TestSynthesis_GetNode 는 노드의 차원별 압박·overall·dominant·top_pods·status 합성을 검증한다.
func TestSynthesis_GetNode(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score", sample(0.78, "node", "worker2")).
		on("node:memory_pressure_score", sample(0.22, "node", "worker2")).
		on("node:pressure_score:5m", sample(0.78, "node", "worker2")).
		on("node:cpu_health_score:5m", sample(0.3, "node", "worker2")).
		on("node:memory_health_score:5m", sample(0.85, "node", "worker2")).
		on("pod:cpu_throttle_score", sample(0.78, "node", "worker2", "src_namespace", "default", "src_pod", "app-x"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Node != "worker2" || resp.DominantDimension != "cpu" || resp.Status != "degraded" {
		t.Errorf("node=%+v want worker2/cpu/degraded", resp)
	}
	if resp.Overall == nil || *resp.Overall != 0.78 {
		t.Errorf("overall=%v want 0.78", resp.Overall)
	}
	if len(resp.TopPods) == 0 || resp.TopPods[0].Pod != "default/app-x" {
		t.Errorf("top_pods=%+v want default/app-x 먼저", resp.TopPods)
	}
	// #264 health 4차원과 신뢰도. health 는 node health rule 값, 신뢰도는 pressure top1(cpu 0.78)
	// 과 top2(memory 0.22) 격차 0.56.
	if resp.Health["cpu"] != 0.3 || resp.Health["memory"] != 0.85 {
		t.Errorf("health=%v want cpu 0.3/memory 0.85", resp.Health)
	}
	if resp.Confidence < 0.559 || resp.Confidence > 0.561 {
		t.Errorf("confidence=%v want ~0.56 (0.78-0.22)", resp.Confidence)
	}
	// #324 등급 동률 (pressure 0.78 → degraded, health 0.3 → degraded) 이면 pressure 로 귀속된다.
	if resp.StatusBasis != "pressure" {
		t.Errorf("status_basis=%q want pressure (동률 우선순위)", resp.StatusBasis)
	}
}

// TestSynthesis_GetNode_WorstOfHealthAndAlert 는 #324 의 gpu 노드 실측 사례 재현이다. dominant
// pressure 가 낮아도 (ok) health.gpu 0.0 과 firing alert 가 있으면 worst-of 합성으로 status 가
// degraded (basis=health) 가 되어, node-map 의 warning 판정과 모순되지 않는다.
func TestSynthesis_GetNode_WorstOfHealthAndAlert(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score", sample(0.12, "node", "gpu")).
		on("node:gpu_health_score", sample(0.0, "node", "gpu")).
		on("node:cpu_health_score", sample(0.9, "node", "gpu")).
		on("ALERTS", sample(1, "alertname", "GPUObsThrottleActive", "severity", "critical", "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != "degraded" || resp.StatusBasis != "health" {
		t.Errorf("status=%q basis=%q want degraded/health (health.gpu 0.0 이 결정)", resp.Status, resp.StatusBasis)
	}
	if !q.sawQuery(`ALERTS{alertstate="firing",node="gpu"}`) {
		t.Error("ALERTS 를 node 라벨로 조회하지 않음 (overview 의 alertedNodes 매칭 규약)")
	}
	if !strings.Contains(resp.Summary, "health") {
		t.Errorf("summary 에 status 결정 근거 미표기: %q", resp.Summary)
	}
}

// TestSynthesis_GetNode_AlertSeverityGrades 는 pressure 와 health 가 정상일 때 firing alert 의
// severity 가 등급을 가르는지 검증한다 (#325). critical 은 degraded, 그 외 severity 는 warn 이다.
func TestSynthesis_GetNode_AlertSeverityGrades(t *testing.T) {
	cases := []struct {
		name       string
		severity   string
		wantStatus string
	}{
		{"critical-degraded", "critical", "degraded"},
		{"warning-warn", "warning", "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := (&fakeQuerier{}).
				on("node:cpu_pressure_score", sample(0.1, "node", "worker1")).
				on("node:cpu_health_score", sample(0.95, "node", "worker1")).
				on("ALERTS", sample(1, "alertname", "NetObsRetransSpike", "severity", tc.severity, "node", "worker1"))
			h := NewSynthesisHandler(q, nil, nil)
			rec := httptest.NewRecorder()
			h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker1", nil))
			var resp NodeResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp.Status != tc.wantStatus || resp.StatusBasis != "alert" {
				t.Errorf("status=%q basis=%q want %s/alert", resp.Status, resp.StatusBasis, tc.wantStatus)
			}
		})
	}
}

// TestSynthesis_GetNode_UsageSaturation 은 limit 없는 pod 의 CPU 포화처럼 CFS throttle 기반
// pressure 에 잡히지 않는 사용량 포화가 status 에 반영되는지 검증한다 (#325). 점유율 0.95 이상은
// degraded, 0.85 구간은 warn 이며 결정 신호는 usage 다.
func TestSynthesis_GetNode_UsageSaturation(t *testing.T) {
	cases := []struct {
		name       string
		cpuFrac    float64
		wantStatus string
	}{
		{"cpu-saturated-degraded", 0.96, "degraded"},
		{"cpu-high-warn", 0.87, "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := (&fakeQuerier{}).
				on("node:cpu_pressure_score", sample(0.1, "node", "worker1")).
				on("node:cpu_health_score", sample(0.95, "node", "worker1")).
				on("container_cpu_usage_seconds_total", sample(tc.cpuFrac, "node", "worker1")).
				on("container_memory_working_set_bytes", sample(0.4, "node", "worker1"))
			h := NewSynthesisHandler(q, nil, nil)
			rec := httptest.NewRecorder()
			h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker1", nil))
			var resp NodeResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp.Status != tc.wantStatus || resp.StatusBasis != "usage" {
				t.Errorf("status=%q basis=%q want %s/usage (cpu 점유율 %.2f)", resp.Status, resp.StatusBasis, tc.wantStatus, tc.cpuFrac)
			}
			if !q.sawQuery("kube_node_status_allocatable") {
				t.Error("allocatable 분모 산식 (#313 재사용) 으로 조회하지 않음")
			}
		})
	}
}

// TestSynthesis_GetNode_NoSignalsUnknown 은 세 신호가 전부 부재일 때 unknown 과 빈 basis 를
// 유지하는지 검증한다.
func TestSynthesis_GetNode_NoSignalsUnknown(t *testing.T) {
	h := NewSynthesisHandler(&fakeQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/ghost", nil))
	var resp NodeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Status != "unknown" || resp.StatusBasis != "" {
		t.Errorf("status=%q basis=%q want unknown/빈 값", resp.Status, resp.StatusBasis)
	}
}

// fakeNeighbors 는 SnapshotSource 테스트 더블이다.
type fakeNeighbors struct{ data []correlation.NoisyNeighbor }

func (f *fakeNeighbors) Snapshot() []correlation.NoisyNeighbor { return f.data }

// TestSynthesis_GetEvents 는 anomaly(z-score)와 noisy-neighbor(causal_strength)를 severity 정렬 사건
// 으로 묶고, min_severity 미만(elevated 기본)을 제외하며 자연어 설명을 다는지 검증한다.
func TestSynthesis_GetEvents(t *testing.T) {
	q := (&fakeQuerier{}).on("cluster:cpu_throttle_zscore", sample(3.2))
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{
		{
			Suspect:   correlation.PodIdentity{Namespace: "default", Pod: "proxy"},
			Victim:    correlation.PodIdentity{Namespace: "default", Pod: "app-x"},
			Dimension: correlation.DimensionNetwork, VictimSignal: correlation.SignalLatency,
			Score: 0.91, CausalStrength: 0.82, GrangerOK: true, PValue: 0.01,
		},
		// causal_strength 0.30 → low → 기본 min_severity=elevated 에서 제외돼야 한다.
		{
			Suspect:   correlation.PodIdentity{Namespace: "default", Pod: "noise"},
			Victim:    correlation.PodIdentity{Namespace: "default", Pod: "app-y"},
			Dimension: correlation.DimensionCPU, CausalStrength: 0.30, Score: 0.4,
		},
	}}
	h := NewSynthesisHandler(q, nb, nil)
	rec := httptest.NewRecorder()
	h.GetEvents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp EventsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	// high noisy-neighbor 1 + high anomaly 1 = 2 (low noisy-neighbor 제외).
	if len(resp.Events) != 2 {
		t.Fatalf("events=%d want 2 (low 제외)", len(resp.Events))
	}
	for _, e := range resp.Events {
		if e.Severity == "low" {
			t.Errorf("low 사건이 포함됨: %+v", e)
		}
	}
	// 정렬: high 둘 중 강도(causal 0.82 vs z 3.2)… 둘 다 high 라 안정 정렬. noisy-neighbor 설명에 상관 포함.
	var foundNN bool
	for _, e := range resp.Events {
		if e.Kind == "noisy_neighbor" {
			foundNN = true
			if e.CausalStrength == nil || *e.CausalStrength != 0.82 {
				t.Errorf("noisy-neighbor causal=%v want 0.82", e.CausalStrength)
			}
			if !strings.Contains(e.Explanation, "상관") {
				t.Errorf("explanation=%q want 상관 포함", e.Explanation)
			}
		}
	}
	if !foundNN {
		t.Errorf("noisy_neighbor 사건 없음: %+v", resp.Events)
	}
	// #237 원인 축 링크 확장. network dimension 의 noisy-neighbor 는 drops 와 latency_breakdown 링크,
	// cpu anomaly 는 detail (pressure) 만. 모든 링크에 관찰 시점 at 이 포함되어 #235 와 결합된다.
	for _, e := range resp.Events {
		if e.Links["detail"] == "" || !strings.Contains(e.Links["detail"], "at="+resp.GeneratedAt) {
			t.Errorf("kind=%s detail=%q want at=%s 포함", e.Kind, e.Links["detail"], resp.GeneratedAt)
		}
		switch e.Kind {
		case "noisy_neighbor": // dimension=network
			if !strings.Contains(e.Links["drops"], "/api/v1/drops?at=") || !strings.Contains(e.Links["latency_breakdown"], "/api/v1/latency-breakdown?at=") {
				t.Errorf("network links=%v want drops/latency_breakdown", e.Links)
			}
		case "anomaly": // dimension=cpu
			if len(e.Links) != 1 {
				t.Errorf("cpu links=%v want detail 만", e.Links)
			}
		}
	}
}

// TestCauseLinks_Dimensions 는 gpu 와 memory dimension 의 원인 축 링크 셋을 검증한다.
func TestCauseLinks_Dimensions(t *testing.T) {
	gpu := causeLinks("gpu", "2026-07-08T00:00:00Z", "/api/v1/pressure?dimension=gpu&scope=pod")
	if gpu["gpu_idle"] != "/api/v1/gpu-idle?at=2026-07-08T00:00:00Z" || gpu["gpu_status"] != "/api/v1/gpu-status?at=2026-07-08T00:00:00Z" {
		t.Errorf("gpu links=%v", gpu)
	}
	if gpu["detail"] != "/api/v1/pressure?dimension=gpu&scope=pod&at=2026-07-08T00:00:00Z" {
		t.Errorf("gpu detail=%q want 기존 쿼리에 & 로 결합", gpu["detail"])
	}
	mem := causeLinks("memory", "2026-07-08T00:00:00Z", "/api/v1/pressure?dimension=memory&scope=pod")
	if mem["memory"] != "/api/v1/memory?at=2026-07-08T00:00:00Z" {
		t.Errorf("memory links=%v", mem)
	}
}

// TestSynthesis_GetHealth_NaN 은 health·node pressure 가 NaN 으로 와도 (division-by-zero recording
// rule 등) JSON 직렬화가 실패하지 않고 unknown/hotspot nil 로 graceful 처리되는지 검증한다. NaN 은
// json.Marshal 이 거부하므로 가드 누락 시 전체 응답이 깨진다.
func TestSynthesis_GetHealth_NaN(t *testing.T) {
	q := (&fakeQuerier{}).
		on("cluster:cpu_health_score", sample(math.NaN())).
		on("node:cpu_pressure_score", sample(math.NaN(), "node", "worker2")).
		on("cluster:cpu_throttle_zscore", sample(math.NaN()))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetHealth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode (NaN 으로 marshal 깨짐?): %v", err)
	}
	cpu := resp.Dimensions["cpu"]
	if cpu.Status != "unknown" || cpu.Health != nil || cpu.Hotspot != nil {
		t.Errorf("cpu=%+v want unknown/nil health/nil hotspot (NaN graceful)", cpu)
	}
	// NaN z-score 는 anomaly 로 만들어지면 안 된다 (가드 + ZScoreSeverity none 이중 안전).
	if len(resp.Anomalies) != 0 {
		t.Errorf("anomalies=%+v want 0 (NaN z-score 제외)", resp.Anomalies)
	}
}

// TestSynthesis_GetPressure_NaN 은 NaN 샘플이 랭킹 전에 걸러져 정상 값만 직렬화되는지 검증한다.
func TestSynthesis_GetPressure_NaN(t *testing.T) {
	q := (&fakeQuerier{}).on("pod:cpu_throttle_score",
		sample(math.NaN(), "node", "worker2", "src_namespace", "default", "src_pod", "nan-pod"),
		sample(0.62, "node", "worker2", "src_namespace", "default", "src_pod", "app-x"),
	)
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=cpu&scope=pod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp PressureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode (NaN 으로 marshal 깨짐?): %v", err)
	}
	if len(resp.Ranking) != 1 || resp.Ranking[0].Pod != "default/app-x" {
		t.Errorf("ranking=%+v want NaN 제외 후 app-x 1건", resp.Ranking)
	}
}

// TestSynthesis_GetHealth_NilQuerier 는 querier 가 nil 일 때 panic 없이 unknown 응답을 돌려주는지
// 검증한다.
func TestSynthesis_GetHealth_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetHealth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp HealthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Dimensions["cpu"].Status != "unknown" || resp.Dimensions["cpu"].Health != nil {
		t.Errorf("cpu=%+v want unknown/nil health (nil querier)", resp.Dimensions["cpu"])
	}
}
