package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"netobs/internal/correlation"
)

// incidentsSeries 는 range [now-1h, now] 를 가정한 ALERTS 시리즈 픽스처다. fakeFetcher 의 gotRange
// 기반이 아니라 buildIncidents 가 절대 시각으로 판정하므로 now 기준으로 timestamps 를 구성한다.
func incidentsSeries(now time.Time) []correlation.LabeledSeries {
	m := func(t time.Time) int64 { return t.UnixMilli() }
	return []correlation.LabeledSeries{
		{
			// 발화 중 (range 끝까지 샘플): firing 판정.
			Series: correlation.TimeSeries{
				Labels: map[string]string{"alertname": "GPUIdleWithMemoryPressure", "severity": "warning", "component": "correlation", "node": "gpu", "job": "x"},
				Samples: []correlation.Sample{
					{TimestampMs: m(now.Add(-10 * time.Minute)), Value: 1},
					{TimestampMs: m(now.Add(-5 * time.Minute)), Value: 1},
					{TimestampMs: m(now.Add(-1 * time.Minute)), Value: 1},
				},
			},
		},
		{
			// 재발화: 간극 (>2*step=10m) 으로 에피소드 2개, 앞 에피소드는 resolved.
			Series: correlation.TimeSeries{
				Labels: map[string]string{"alertname": "NetObsDropBurst", "severity": "critical", "component": "netobs"},
				Samples: []correlation.Sample{
					{TimestampMs: m(now.Add(-50 * time.Minute)), Value: 1},
					{TimestampMs: m(now.Add(-45 * time.Minute)), Value: 1},
					{TimestampMs: m(now.Add(-20 * time.Minute)), Value: 1},
					{TimestampMs: m(now.Add(-15 * time.Minute)), Value: 1},
				},
			},
		},
	}
}

// TestIncidents 는 에피소드 분리 (재발화) 와 firing/resolved 판정, 라벨 필터, 최근 우선 정렬을
// 검증한다. step=5m 라 간극 임계는 10m 다.
func TestIncidents(t *testing.T) {
	now := time.Now()
	f := &fakeFetcher{series: incidentsSeries(now)}
	h := NewIncidentsHandler(f)
	rec := httptest.NewRecorder()
	h.GetIncidents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents?step=5m", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp IncidentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Incidents) != 3 {
		t.Fatalf("incidents=%d want 3 (firing 1 + 재발화 분리 2): %+v", len(resp.Incidents), resp.Incidents)
	}
	// 최근 발화 우선: GPUIdle(-10m) → DropBurst 재발화(-20m) → DropBurst 최초(-50m)
	if resp.Incidents[0].Alertname != "GPUIdleWithMemoryPressure" || resp.Incidents[0].Status != "firing" {
		t.Errorf("incidents[0]=%+v want GPUIdle firing", resp.Incidents[0])
	}
	if resp.Incidents[0].EndsAt != "" {
		t.Errorf("firing 인데 ends_at=%q", resp.Incidents[0].EndsAt)
	}
	if resp.Incidents[1].Alertname != "NetObsDropBurst" || resp.Incidents[1].Status != "resolved" || resp.Incidents[1].EndsAt == "" {
		t.Errorf("incidents[1]=%+v want DropBurst resolved + ends_at", resp.Incidents[1])
	}
	if resp.Incidents[2].Status != "resolved" {
		t.Errorf("incidents[2]=%+v want 최초 에피소드 resolved", resp.Incidents[2])
	}
	// 라벨 필터: 전용 필드 승격분과 scrape 계열 제외, node 는 유지.
	if resp.Incidents[0].Labels["node"] != "gpu" {
		t.Errorf("labels=%v want node 유지", resp.Incidents[0].Labels)
	}
	if _, ok := resp.Incidents[0].Labels["job"]; ok {
		t.Errorf("labels=%v want job 제외", resp.Incidents[0].Labels)
	}
	// starts_at 은 at 파라미터에 그대로 결합 가능한 RFC3339.
	if _, err := time.Parse(time.RFC3339, resp.Incidents[0].StartsAt); err != nil {
		t.Errorf("starts_at=%q RFC3339 아님: %v", resp.Incidents[0].StartsAt, err)
	}
}

// TestIncidents_Truncated 는 range 시작부터 발화 중이던 에피소드가 절단 표시되는지 검증한다.
func TestIncidents_Truncated(t *testing.T) {
	now := time.Now()
	f := &fakeFetcher{series: []correlation.LabeledSeries{{
		Series: correlation.TimeSeries{
			Labels: map[string]string{"alertname": "OldAlert"},
			Samples: []correlation.Sample{
				{TimestampMs: now.Add(-59 * time.Minute).UnixMilli(), Value: 1},
				{TimestampMs: now.Add(-30 * time.Minute).UnixMilli(), Value: 1},
			},
		},
	}}}
	h := NewIncidentsHandler(f)
	rec := httptest.NewRecorder()
	h.GetIncidents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents?range=1h&step=30m", nil))
	var resp IncidentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Incidents) != 1 || !resp.Incidents[0].Truncated {
		t.Errorf("incidents=%+v want truncated=true (range 시작 이전부터 발화)", resp.Incidents)
	}
}

// TestIncidents_StableTieOrder 는 동일 시각 동일 alertname 의 다건 발화 (라벨만 상이) 가 입력
// 시리즈 순서를 보존하는지 검증한다. 불안정 정렬이면 호출마다 순서가 흔들릴 수 있다.
func TestIncidents_StableTieOrder(t *testing.T) {
	now := time.Now()
	ts := now.Add(-10 * time.Minute).UnixMilli()
	mk := func(node string) correlation.LabeledSeries {
		return correlation.LabeledSeries{Series: correlation.TimeSeries{
			Labels:  map[string]string{"alertname": "SameAlert", "node": node},
			Samples: []correlation.Sample{{TimestampMs: ts, Value: 1}},
		}}
	}
	f := &fakeFetcher{series: []correlation.LabeledSeries{mk("a"), mk("b"), mk("c")}}
	h := NewIncidentsHandler(f)
	rec := httptest.NewRecorder()
	h.GetIncidents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents?step=5m", nil))
	var resp IncidentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Incidents) != 3 {
		t.Fatalf("incidents=%d want 3", len(resp.Incidents))
	}
	for i, want := range []string{"a", "b", "c"} {
		if resp.Incidents[i].Labels["node"] != want {
			t.Errorf("incidents[%d].node=%q want %q (입력 순서 보존)", i, resp.Incidents[i].Labels["node"], want)
		}
	}
}

// TestIncidents_PodNodeFilter 는 #248 의 node / pod 필터가 라벨 규약 3종 (pod / src_pod /
// victim_pod) 매칭으로 에피소드를 좁히는지 검증한다.
func TestIncidents_PodNodeFilter(t *testing.T) {
	now := time.Now()
	ts := now.Add(-10 * time.Minute).UnixMilli()
	mk := func(labels map[string]string) correlation.LabeledSeries {
		return correlation.LabeledSeries{Series: correlation.TimeSeries{
			Labels:  labels,
			Samples: []correlation.Sample{{TimestampMs: ts, Value: 1}},
		}}
	}
	f := &fakeFetcher{series: []correlation.LabeledSeries{
		mk(map[string]string{"alertname": "NetObsDropBurst", "node": "gpu", "src_pod": "trainer"}),
		mk(map[string]string{"alertname": "CorrelationStrongNoisyNeighbor", "victim_pod": "trainer"}),
		mk(map[string]string{"alertname": "GPUIdleWithMemoryPressure", "node": "worker1", "pod": "other"}),
	}}
	h := NewIncidentsHandler(f)

	get := func(target string) IncidentsResponse {
		rec := httptest.NewRecorder()
		h.GetIncidents(rec, httptest.NewRequest(http.MethodGet, target, nil))
		var resp IncidentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	// pod 필터: src_pod 와 victim_pod 규약 모두 매칭돼 2건.
	resp := get("/api/v1/incidents?pod=trainer")
	if len(resp.Incidents) != 2 {
		t.Fatalf("pod=trainer incidents=%d want 2: %+v", len(resp.Incidents), resp.Incidents)
	}
	// node 필터: node=gpu 1건.
	resp = get("/api/v1/incidents?node=gpu")
	if len(resp.Incidents) != 1 || resp.Incidents[0].Alertname != "NetObsDropBurst" {
		t.Fatalf("node=gpu incidents=%+v want NetObsDropBurst 1건", resp.Incidents)
	}
	// 미매칭 필터는 빈 결과.
	if resp = get("/api/v1/incidents?pod=absent"); len(resp.Incidents) != 0 {
		t.Errorf("pod=absent incidents=%d want 0", len(resp.Incidents))
	}
}

// TestIncidents_InvalidRange 는 파싱 불가 range 가 400 인지 검증한다.
func TestIncidents_InvalidRange(t *testing.T) {
	h := NewIncidentsHandler(&fakeFetcher{})
	rec := httptest.NewRecorder()
	h.GetIncidents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents?range=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// TestIncidents_NilFetcher 는 fetcher 미주입 시 빈 응답을 graceful 하게 돌려주는지 검증한다.
func TestIncidents_NilFetcher(t *testing.T) {
	h := NewIncidentsHandler(nil)
	rec := httptest.NewRecorder()
	h.GetIncidents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/incidents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp IncidentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Incidents) != 0 {
		t.Errorf("incidents=%d want 0", len(resp.Incidents))
	}
}
