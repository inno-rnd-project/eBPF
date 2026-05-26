package sources

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"netobs/internal/rca/registry"
)

// TestHttpSnapshotSource_FetchAndCache 는 첫 호출에서 HTTP fetch 가 일어나고 cache TTL 안에서는
// 동일 결과가 다시 fetch 없이 돌려지는지 검증한다.
func TestHttpSnapshotSource_FetchAndCache(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{
			"victim": {"namespace":"default","pod":"v","pod_uid":"uv"},
			"suspect": {"namespace":"noisy","pod":"s","pod_uid":"us"},
			"dimension": "cpu",
			"score": 0.9
		}]`))
	}))
	defer srv.Close()

	src := newHTTPSnapshotSource(srv.URL, time.Second, time.Minute)

	first := src.fetch()
	if len(first) != 1 {
		t.Fatalf("first fetch len=%d; want 1", len(first))
	}
	if first[0].SuspectPod != "s" || first[0].Dimension != "cpu" {
		t.Errorf("first[0]=%+v", first[0])
	}

	second := src.fetch()
	if len(second) != 1 {
		t.Errorf("second fetch len=%d", len(second))
	}
	if calls != 1 {
		t.Errorf("HTTP calls=%d; want 1 (cache hit on second)", calls)
	}
}

// TestHttpSnapshotSource_FetchErrorReturnsStaleOrEmpty 는 backing HTTP 가 실패할 때 cache 의
// stale 값을 돌려주고 cache 가 비었으면 nil 을 돌려주는지 검증한다.
func TestHttpSnapshotSource_FetchErrorReturnsStaleOrEmpty(t *testing.T) {
	src := newHTTPSnapshotSource("http://127.0.0.1:1", 50*time.Millisecond, time.Minute)
	got := src.fetch()
	if got != nil {
		t.Errorf("fetch=%v with no cache; want nil", got)
	}
}

// TestSources_TopNeighborsFiltersByVictim 은 Sources 가 snapshot 에서 victim 매칭 entry 만 잘라
// registry.NeighborInfo 로 변환하는지 검증한다.
func TestSources_TopNeighborsFiltersByVictim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"victim":{"namespace":"default","pod":"v","pod_uid":"uv"}, "suspect":{"namespace":"noisy","pod":"s1","pod_uid":"us1"}, "dimension":"cpu", "score":0.9},
			{"victim":{"namespace":"default","pod":"v","pod_uid":"uv"}, "suspect":{"namespace":"noisy","pod":"s2","pod_uid":"us2"}, "dimension":"gpu", "score":0.7},
			{"victim":{"namespace":"other","pod":"x","pod_uid":"ux"}, "suspect":{"namespace":"noisy","pod":"s3","pod_uid":"us3"}, "dimension":"network", "score":0.6}
		]`))
	}))
	defer srv.Close()

	s := New(srv.URL, "", time.Second, time.Minute, 5)
	got := s.TopNeighbors("default", "v")
	if len(got) != 2 {
		t.Fatalf("got %d neighbors; want 2 (other victim filtered)", len(got))
	}
	if got[0].SuspectPod != "s1" || got[1].SuspectPod != "s2" {
		t.Errorf("neighbors=%+v", got)
	}
}

// TestSources_TopNeighborsSortedByScoreDesc 는 correlation-exporter snapshot 이 (victim, dimension,
// rank) 그룹 정렬이라 같은 victim 의 dimension 사전순 첫 entry 가 가장 강한 score 가 아닐 수 있는
// 케이스에서 TopNeighbors 가 score 절대값 내림차순으로 재정렬해 진짜 dominant suspect 를 [0] 으로
// 노출하는지 검증한다.
func TestSources_TopNeighborsSortedByScoreDesc(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// snapshot 등장 순서가 (victim, dimension, rank) 정렬이라 weakSuspect 가 strongSuspect 보다
		// 먼저 등장하는 시나리오. 정렬 없이 단순 break 하면 [0] 이 weakSuspect 가 된다.
		_, _ = w.Write([]byte(`[
			{"victim":{"namespace":"default","pod":"v","pod_uid":"uv"}, "suspect":{"namespace":"noisy","pod":"weak","pod_uid":"u-weak"}, "dimension":"cpu", "score":0.4},
			{"victim":{"namespace":"default","pod":"v","pod_uid":"uv"}, "suspect":{"namespace":"noisy","pod":"strong","pod_uid":"u-strong"}, "dimension":"memory", "score":0.95}
		]`))
	}))
	defer srv.Close()

	s := New(srv.URL, "", time.Second, time.Minute, 5)
	got := s.TopNeighbors("default", "v")
	if len(got) != 2 {
		t.Fatalf("got %d neighbors; want 2", len(got))
	}
	if got[0].SuspectPod != "strong" {
		t.Errorf("got[0].SuspectPod=%q; want strong (highest score)", got[0].SuspectPod)
	}
	if got[0].Score != 0.95 {
		t.Errorf("got[0].Score=%v; want 0.95", got[0].Score)
	}
}

// TestHttpPromQLSource_DecodesTopkResult 는 Prometheus instant query 응답의 vector 형식이 정확히
// DropFlowInfo 슬라이스로 변환되는지 검증한다.
func TestHttpPromQLSource_DecodesTopkResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "topk") {
			t.Errorf("missing topk in query: %s", r.URL.RawQuery)
		}
		body := map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []any{
					map[string]any{
						"metric": map[string]string{
							"src_namespace": "ns1", "src_pod": "p1",
							"dst_ip": "10.0.0.5", "dst_port": "8080",
							"protocol": "tcp", "drop_reason": "TCP_RESET",
						},
						"value": []any{float64(1700000000), "15.5"},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	p := newHTTPPromQLSource(srv.URL, time.Second)
	flows := p.fetchTopDropFlows("ns1", 5)
	if len(flows) != 1 {
		t.Fatalf("got %d flows; want 1", len(flows))
	}
	if flows[0].DstIP != "10.0.0.5" || flows[0].RatePerSec != 15.5 {
		t.Errorf("flows[0]=%+v", flows[0])
	}
}

// TestHttpPromQLSource_FetchErrorReturnsNil 은 Prometheus unreachable 케이스에서 nil 을 돌려주는지
// 검증한다.
func TestHttpPromQLSource_FetchErrorReturnsNil(t *testing.T) {
	p := newHTTPPromQLSource("http://127.0.0.1:1", 50*time.Millisecond)
	if got := p.fetchTopDropFlows("ns", 5); got != nil {
		t.Errorf("got=%v; want nil on unreachable", got)
	}
}

// TestSources_TopDropFlowsForwards 는 Sources 가 promql source 결과를 그대로 forwarding 하는지
// 검증한다 (간이 통합).
func TestSources_TopDropFlowsForwards(t *testing.T) {
	s := &Sources{snapshot: noopSnapshot{}, promql: noopPromQL{}, topN: 5}
	if got := s.TopDropFlows("ns"); got != nil {
		t.Errorf("noop got=%v; want nil", got)
	}

	// fake promql 결과 주입
	s.promql = stubPromQL{flows: []registry.DropFlowInfo{{SrcPod: "p1"}, {SrcPod: "p2"}}}
	got := s.TopDropFlows("ns")
	if len(got) != 2 || got[0].SrcPod != "p1" {
		t.Errorf("got=%+v", got)
	}
}

type stubPromQL struct {
	flows []registry.DropFlowInfo
}

func (s stubPromQL) fetchTopDropFlows(string, int) []registry.DropFlowInfo {
	return s.flows
}
