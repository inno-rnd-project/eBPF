package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"netobs/internal/correlation"
)

// fakeQuerier 는 query 문자열별 canned InstantSample 을 돌려주는 테스트 더블이다. 부분 문자열 매칭으로
// topk wrapping 이나 node 필터가 붙은 query 도 같은 규칙에 걸리게 한다.
type fakeQuerier struct {
	rules []struct {
		contains string
		samples  []correlation.InstantSample
	}
}

func (f *fakeQuerier) on(contains string, samples ...correlation.InstantSample) *fakeQuerier {
	f.rules = append(f.rules, struct {
		contains string
		samples  []correlation.InstantSample
	}{contains, samples})
	return f
}

func (f *fakeQuerier) Query(_ context.Context, query string) ([]correlation.InstantSample, error) {
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

	h := NewSynthesisHandler(q, nil)
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
}

// TestSynthesis_GetPressure 는 dimension=cpu&scope=pod 가 pressure 내림차순 랭킹을 rank·severity 와
// 함께 돌려주는지 검증한다.
func TestSynthesis_GetPressure(t *testing.T) {
	q := (&fakeQuerier{}).on("pod:cpu_throttle_score",
		sample(0.51, "node", "worker2", "src_namespace", "batch", "src_pod", "job-7"),
		sample(0.78, "node", "worker2", "src_namespace", "default", "src_pod", "app-x"),
	)
	h := NewSynthesisHandler(q, nil)
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
	h := NewSynthesisHandler(&fakeQuerier{}, nil)
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
		on("pod:cpu_throttle_score", sample(0.78, "node", "worker2", "src_namespace", "default", "src_pod", "app-x"))
	h := NewSynthesisHandler(q, nil)
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
	h := NewSynthesisHandler(q, nb)
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
}

// TestSynthesis_GetHealth_NilQuerier 는 querier 가 nil 일 때 panic 없이 unknown 응답을 돌려주는지
// 검증한다.
func TestSynthesis_GetHealth_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil)
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
