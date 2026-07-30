package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	// failErr 가 설정되면 Query 가 항상 이 error 를 돌려준다 (#352 query_failed 500 규약 테스트용,
	// Prometheus 백엔드 장애 재현). failOn 이 설정되면 query 에 failOn 이 포함된 경우에만 error 를
	// 낸다 (부분 실패 재현: overview 의 부가 health 쿼리만 실패시켜 degrade 검증).
	failErr error
	failOn  string
}

// failing 은 Query 가 항상 error 를 내는 fakeQuerier 를 만든다 (백엔드 장애 재현).
func failingQuerier() *fakeQuerier {
	return &fakeQuerier{failErr: fmt.Errorf("prometheus unreachable")}
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
	failErr := f.failErr
	failOn := f.failOn
	f.mu.Unlock()
	if failErr != nil {
		return nil, failErr
	}
	if failOn != "" && strings.Contains(query, failOn) {
		return nil, fmt.Errorf("query failed: %s", failOn)
	}
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

// TestSynthesis_GetNode_MemoryPressureUsageScale 은 memory 차원 pressure 의 usage 임계 환산을
// 검증한다. memory 의 node pressure 는 실측 사용률이라 일반 임계 (0.4) 로는 정상 상주 사용률
// (0.58) 이 warn 으로 과민 판정되어 node-map 의 healthy 와 모순됐다. 0.85 미만은 ok, 0.87 은
// warn 이며, memory 재척도로 dominant (최대값) 차원과 최악 등급이 어긋나는 케이스 (memory 0.6
// dominant + cpu 0.45 elevated) 에서 전 차원 등급화가 cpu 의 warn 을 잡는다.
func TestSynthesis_GetNode_MemoryPressureUsageScale(t *testing.T) {
	cases := []struct {
		name       string
		memory     float64
		cpu        float64
		wantStatus string
	}{
		{"memory-58pct-ok", 0.58, 0.10, "ok"},
		{"memory-87pct-warn", 0.87, 0.10, "warn"},
		{"non-dominant-cpu-elevated-warn", 0.60, 0.45, "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := (&fakeQuerier{}).
				on("node:memory_pressure_score", sample(tc.memory, "node", "worker2")).
				on("node:cpu_pressure_score", sample(tc.cpu, "node", "worker2")).
				on("node:cpu_health_score", sample(0.95, "node", "worker2"))
			h := NewSynthesisHandler(q, nil, nil)
			rec := httptest.NewRecorder()
			h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker2", nil))
			var resp NodeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Status != tc.wantStatus || resp.StatusBasis != "pressure" {
				t.Errorf("status=%q basis=%q want %s/pressure (memory %.2f, cpu %.2f)", resp.Status, resp.StatusBasis, tc.wantStatus, tc.memory, tc.cpu)
			}
		})
	}
}

// TestSynthesis_GetHealth_MemoryHotspotUsageScale 은 #359 회귀 가드다. /health 의 memory hotspot
// severity 가 usage 임계 (0.85/0.95) 로 환산돼, 같은 노드·같은 값에 대해 /node 의 memory 등급과 동일
// tier 를 내는지 검증한다. Hotspot.Severity 어휘 (low/elevated/high) 와 node status 어휘
// (ok/warn/degraded) 는 다르므로 tier 대응 (low↔ok, elevated↔warn, high↔degraded) 으로 대조한다.
// 일반 임계 (0.4) 를 쓰던 기존 코드에서는 memory 0.60 이 elevated 로 떠 /node 의 ok 와 어긋났다.
func TestSynthesis_GetHealth_MemoryHotspotUsageScale(t *testing.T) {
	cases := []struct {
		name         string
		memory       float64
		wantSeverity string // /health hotspot
		wantNodeTier string // /node memory 등급
	}{
		{"memory-60pct-low", 0.60, "low", "ok"},
		{"memory-88pct-elevated", 0.88, "elevated", "warn"},
		{"memory-97pct-high", 0.97, "high", "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// /health: memory 차원 hotspot severity.
			qh := (&fakeQuerier{}).on("node:memory_pressure_score", sample(tc.memory, "node", "worker2"))
			recH := httptest.NewRecorder()
			NewSynthesisHandler(qh, nil, nil).GetHealth(recH, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
			var health HealthResponse
			if err := json.Unmarshal(recH.Body.Bytes(), &health); err != nil {
				t.Fatalf("health decode: %v", err)
			}
			hs := health.Dimensions["memory"].Hotspot
			if hs == nil || hs.Severity != tc.wantSeverity {
				t.Fatalf("/health memory hotspot=%+v want severity %q (memory %.2f)", hs, tc.wantSeverity, tc.memory)
			}

			// /node: 같은 memory 값의 등급. cpu·health·alert 미설정이라 status 는 memory pressure 등급.
			qn := (&fakeQuerier{}).on("node:memory_pressure_score", sample(tc.memory, "node", "worker2"))
			recN := httptest.NewRecorder()
			NewSynthesisHandler(qn, nil, nil).GetNode(recN, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker2", nil))
			var node NodeResponse
			if err := json.Unmarshal(recN.Body.Bytes(), &node); err != nil {
				t.Fatalf("node decode: %v", err)
			}
			if node.Status != tc.wantNodeTier {
				t.Fatalf("/node status=%q want %q (memory %.2f)", node.Status, tc.wantNodeTier, tc.memory)
			}
		})
	}
}

// TestSynthesis_GetHealth_MemoryDoesNotStealDominant 은 #359 의 dominant 정합을 검증한다. 60% 사용
// 중인 memory (usage 임계로 low) 가 raw 값 (0.60) 만으로 throttle 기반 cpu (0.45, elevated) 를 눌러
// DominantPressure 를 차지하던 문제가, severity 우선 비교로 cpu 에 귀속되는지 확인한다.
func TestSynthesis_GetHealth_MemoryDoesNotStealDominant(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score", sample(0.45, "node", "worker2")).
		on("node:memory_pressure_score", sample(0.60, "node", "worker2"))
	rec := httptest.NewRecorder()
	NewSynthesisHandler(q, nil, nil).GetHealth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Dimensions["memory"].Hotspot == nil || resp.Dimensions["memory"].Hotspot.Severity != "low" {
		t.Errorf("memory hotspot=%+v want severity low", resp.Dimensions["memory"].Hotspot)
	}
	if resp.DominantPressure == nil || resp.DominantPressure.Dimension != "cpu" {
		t.Errorf("dominant=%+v want cpu (memory low 가 cpu elevated 를 누르면 안 됨)", resp.DominantPressure)
	}
}

// TestSynthesis_GetHealth_DominantTieBreakScaleNeutral 은 #359 리뷰의 동률 tie-break 정합을 검증한다.
// memory usage 0.87 과 cpu throttle 0.45 는 둘 다 elevated 동률이지만, raw 값 tie-break 이면 memory 가
// 0.87 > 0.45 로 dominant 를 선점한다. severityProgress (자기 임계 대비 상대 위치) tie-break 로는
// cpu (0.45/0.4 = 1.125) 가 memory (0.87/0.85 = 1.024) 보다 커 dominant 가 cpu 에 귀속돼야 한다.
func TestSynthesis_GetHealth_DominantTieBreakScaleNeutral(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score", sample(0.45, "node", "worker2")).
		on("node:memory_pressure_score", sample(0.87, "node", "worker2"))
	rec := httptest.NewRecorder()
	NewSynthesisHandler(q, nil, nil).GetHealth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 둘 다 elevated 동률임을 먼저 확인해 tie-break 경로가 실제로 자극되는지 보장한다.
	if resp.Dimensions["cpu"].Hotspot == nil || resp.Dimensions["cpu"].Hotspot.Severity != "elevated" {
		t.Fatalf("cpu hotspot=%+v want severity elevated", resp.Dimensions["cpu"].Hotspot)
	}
	if resp.Dimensions["memory"].Hotspot == nil || resp.Dimensions["memory"].Hotspot.Severity != "elevated" {
		t.Fatalf("memory hotspot=%+v want severity elevated", resp.Dimensions["memory"].Hotspot)
	}
	if resp.DominantPressure == nil || resp.DominantPressure.Dimension != "cpu" {
		t.Errorf("dominant=%+v want cpu (동률 elevated 에서 척도 중립 tie-break 으로 cpu 귀속)", resp.DominantPressure)
	}
}

// TestSynthesis_GetPressure_MemoryScaleByScope 은 #359 리뷰의 /pressure scope 별 memory 척도를 검증한다.
// scope=node 는 node:memory_pressure_score (실측 사용률) 라 usage 임계로 60% 가 low 여야 /health · /node
// 와 정합하고, scope=pod 는 pod:memory_pressure_score (OOM 근접도) 라 일반 임계로 60% 가 elevated 를
// 유지해야 한다.
func TestSynthesis_GetPressure_MemoryScaleByScope(t *testing.T) {
	t.Run("node-usage-scale", func(t *testing.T) {
		q := (&fakeQuerier{}).on("node:memory_pressure_score", sample(0.60, "node", "worker2"))
		rec := httptest.NewRecorder()
		NewSynthesisHandler(q, nil, nil).GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=memory&scope=node&limit=10", nil))
		var resp PressureResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Ranking) != 1 || resp.Ranking[0].Severity != "low" {
			t.Errorf("ranking=%+v want severity low (usage 60%% < 0.85)", resp.Ranking)
		}
	})
	t.Run("pod-oom-scale", func(t *testing.T) {
		q := (&fakeQuerier{}).on("pod:memory_pressure_score", sample(0.60, "node", "worker2", "src_namespace", "ns", "src_pod", "p"))
		rec := httptest.NewRecorder()
		NewSynthesisHandler(q, nil, nil).GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=memory&scope=pod&limit=10", nil))
		var resp PressureResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if len(resp.Ranking) != 1 || resp.Ranking[0].Severity != "elevated" {
			t.Errorf("ranking=%+v want severity elevated (OOM 근접도는 일반 임계 유지)", resp.Ranking)
		}
	})
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

// TestSynthesis_GetNode_AlertSeverityGrades 는 pressure 와 health 가 정상일 때 지속성 게이트를 통과한
// firing alert 의 severity 가 등급을 가르는지 검증한다 (#325, #379). critical 은 degraded, 그 외
// severity 는 warn 이며, status 를 올린 alertname 이 StatusAlerts 로 노출된다.
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
				// #379 active-age (초). ALERTS_FOR_STATE 스텁을 ALERTS 보다 먼저 등록해
				// time() - ALERTS_FOR_STATE 쿼리가 이 값을 받게 한다 (substring 매칭 우선). 3600s 는
				// alertStatusMinHold 를 넘겨 지속성 게이트를 통과한다.
				on("ALERTS_FOR_STATE", sample(3600, "alertname", "NetObsRetransSpike", "severity", tc.severity, "node", "worker1")).
				on("ALERTS", sample(1, "alertname", "NetObsRetransSpike", "severity", tc.severity, "node", "worker1"))
			h := NewSynthesisHandler(q, nil, nil)
			rec := httptest.NewRecorder()
			h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker1", nil))
			var resp NodeResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if resp.Status != tc.wantStatus || resp.StatusBasis != "alert" {
				t.Errorf("status=%q basis=%q want %s/alert", resp.Status, resp.StatusBasis, tc.wantStatus)
			}
			if len(resp.StatusAlerts) != 1 || resp.StatusAlerts[0] != "NetObsRetransSpike" {
				t.Errorf("status_alerts=%v want [NetObsRetransSpike] (status 근거 노출)", resp.StatusAlerts)
			}
			if !strings.Contains(resp.Summary, "NetObsRetransSpike") {
				t.Errorf("summary=%q want alertname 명시", resp.Summary)
			}
		})
	}
}

// TestSynthesis_GetNode_AlertPersistenceGate 는 #379 의 핵심 회귀 가드다. pressure/usage 가 정상일 때
// firing alert 가 (1) active-age 가 alertStatusMinHold 미만이면 transient 라 status 에 반영되지 않아
// status=ok 를 유지하고 (flapping 억제), (2) 지속 시간을 넘기면 warn(basis alert) 으로 반영되는지를
// 같은 alert 로 대조한다. 두 케이스의 차이는 오직 active-age 라 게이트 자체를 격리 검증한다.
func TestSynthesis_GetNode_AlertPersistenceGate(t *testing.T) {
	build := func(ageSeconds float64) NodeResponse {
		q := (&fakeQuerier{}).
			on("node:cpu_pressure_score", sample(0.1, "node", "worker1")).
			on("node:cpu_health_score", sample(0.95, "node", "worker1")).
			on("ALERTS_FOR_STATE", sample(ageSeconds, "alertname", "NetObsBpfMapUtilizationHigh", "severity", "warning", "node", "worker1")).
			on("ALERTS", sample(1, "alertname", "NetObsBpfMapUtilizationHigh", "severity", "warning", "node", "worker1"))
		rec := httptest.NewRecorder()
		NewSynthesisHandler(q, nil, nil).GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker1", nil))
		var resp NodeResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp
	}
	// transient (60s < 10m): status 에 반영 안 됨 → pressure 기준 ok, StatusAlerts 생략.
	if r := build(60); r.Status != "ok" || r.StatusBasis == "alert" || len(r.StatusAlerts) != 0 {
		t.Errorf("transient alert: status=%q basis=%q alerts=%v want ok/비alert/생략", r.Status, r.StatusBasis, r.StatusAlerts)
	}
	// sustained (1200s > 10m): warn(basis alert) 로 반영 + StatusAlerts 노출.
	if r := build(1200); r.Status != "warn" || r.StatusBasis != "alert" || len(r.StatusAlerts) != 1 {
		t.Errorf("sustained alert: status=%q basis=%q alerts=%v want warn/alert/[1건]", r.Status, r.StatusBasis, r.StatusAlerts)
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

// TestQueryFailed500Contract 는 #352 의 오류 응답 규약 통일을 검증한다. Prometheus 백엔드 장애
// (Query error) 시 queryParallel 을 쓰는 전 엔드포인트가 200+빈데이터가 아니라 500 query_failed 를
// 돌려줘야 한다. 각 호출 패턴 (단일 queryParallel, 직접 primary + 보조 queryParallel, 다중
// queryParallel) 을 대표 핸들러로 커버한다.
func TestQueryFailed500Contract(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		invoke func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request)
	}{
		{"overview", "/api/v1/overview", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetOverview(rec, req) }},
		{"node-map", "/api/v1/node-map", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetNodeMap(rec, req) }},
		{"nodes", "/api/v1/nodes", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetNodes(rec, req) }},
		{"pods", "/api/v1/pods", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetPods(rec, req) }},
		{"node-resources", "/api/v1/node/n1/resources", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.GetNodeResources(rec, req)
		}},
		{"node-pods", "/api/v1/node/n1/pods", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetNodePods(rec, req) }},
		{"agents", "/api/v1/agents", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetAgents(rec, req) }},
		{"node-vitals", "/api/v1/node-vitals?node=n1", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) {
			h.GetNodeVitals(rec, req)
		}},
		{"pod-detail", "/api/v1/pod/ns/p", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetPodDetail(rec, req) }},
		{"memory", "/api/v1/memory", func(h *SynthesisHandler, rec *httptest.ResponseRecorder, req *http.Request) { h.GetMemory(rec, req) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewSynthesisHandler(failingQuerier(), nil, nil)
			rec := httptest.NewRecorder()
			tc.invoke(h, rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d want 500 (query_failed 규약)", rec.Code)
			}
			var body apicommonErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error.Code != "query_failed" {
				t.Errorf("code=%q want query_failed", body.Error.Code)
			}
		})
	}
}

// apicommonErrorBody 는 apicommon.ErrorBody 의 test 로컬 미러다 (import cycle 없이 decode).
type apicommonErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// TestListPaginationContract 는 #352 의 리스트 페이지네이션 규약을 검증한다. limit 파싱 불가는
// 400 invalid_limit, 결과가 limit 을 넘으면 total 은 전체 수·truncated=true, 안 넘으면 truncated=false.
func TestListPaginationContract(t *testing.T) {
	// pressure: pod scope 3건에 limit=2 → total 3, truncated true, ranking 2.
	q := (&fakeQuerier{}).on("pod:cpu_throttle_score",
		sample(0.9, "node", "n1", "src_namespace", "a", "src_pod", "p1"),
		sample(0.8, "node", "n1", "src_namespace", "a", "src_pod", "p2"),
		sample(0.7, "node", "n1", "src_namespace", "a", "src_pod", "p3"),
	)
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=cpu&scope=pod&limit=2", nil))
	var pr PressureResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &pr)
	if pr.Total != 3 || !pr.Truncated || len(pr.Ranking) != 2 {
		t.Errorf("pressure total=%d truncated=%v len=%d want 3/true/2", pr.Total, pr.Truncated, len(pr.Ranking))
	}

	// limit 미초과: truncated false, total = 실제 수.
	rec = httptest.NewRecorder()
	h.GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=cpu&scope=pod&limit=10", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &pr)
	if pr.Total != 3 || pr.Truncated || len(pr.Ranking) != 3 {
		t.Errorf("pressure total=%d truncated=%v len=%d want 3/false/3", pr.Total, pr.Truncated, len(pr.Ranking))
	}

	// 파싱 불가 limit → 400 invalid_limit.
	rec = httptest.NewRecorder()
	h.GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=cpu&limit=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status=%d want 400", rec.Code)
	}
	var eb apicommonErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &eb)
	if eb.Error.Code != "invalid_limit" {
		t.Errorf("code=%q want invalid_limit", eb.Error.Code)
	}
}
