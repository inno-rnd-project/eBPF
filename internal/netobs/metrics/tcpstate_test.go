package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// TestTCPStateAggregator_BasicAggregation 은 동일 Pod 에 여러 sample 이 들어왔을 때 min_cwnd /
// max_srtt / min_ssthresh 가 의도대로 골라지는지 검증한다.
func TestTCPStateAggregator_BasicAggregation(t *testing.T) {
	agg := NewTCPStateAggregator()
	l := TCPStateLabels{Namespace: "ns", Pod: "p1", Node: "n1"}

	agg.Observe(l, 100, 5_000, 50)
	agg.Observe(l, 80, 8_000, 60)
	agg.Observe(l, 120, 6_000, 30)

	reg := prometheus.NewRegistry()
	reg.MustRegister(agg)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			got[mf.GetName()] = m.GetGauge().GetValue()
		}
	}
	if got["netobs_tcp_state_min_cwnd"] != 80 {
		t.Errorf("min_cwnd=%v want 80", got["netobs_tcp_state_min_cwnd"])
	}
	if got["netobs_tcp_state_max_srtt_seconds"] != 8_000.0/1e6 {
		t.Errorf("max_srtt=%v want 0.008", got["netobs_tcp_state_max_srtt_seconds"])
	}
	if got["netobs_tcp_state_min_ssthresh"] != 30 {
		t.Errorf("min_ssthresh=%v want 30", got["netobs_tcp_state_min_ssthresh"])
	}
}

// gatherMinCwnd는 registry를 gather해 netobs_tcp_state_min_cwnd의 시리즈 수와 첫 값을 돌려주는
// 테스트 헬퍼다.
func gatherMinCwnd(t *testing.T, reg *prometheus.Registry) (int, float64) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "netobs_tcp_state_min_cwnd" {
			if len(mf.Metric) == 0 {
				return 0, 0
			}
			return len(mf.Metric), mf.Metric[0].GetGauge().GetValue()
		}
	}
	return 0, 0
}

// TestTCPStateAggregator_NonDestructiveCollect는 연속 Collect가 같은 값을 재관측하는지 검증한다
// (#443). 종전 파괴적 리셋은 진단 curl 같은 추가 reader가 정규 scrape의 값을 소거했다.
func TestTCPStateAggregator_NonDestructiveCollect(t *testing.T) {
	agg := NewTCPStateAggregator()
	l := TCPStateLabels{Namespace: "ns", Pod: "p1", Node: "n1"}

	agg.Observe(l, 100, 5_000, 50)
	reg := prometheus.NewRegistry()
	reg.MustRegister(agg)

	for i := 0; i < 3; i++ {
		if n, v := gatherMinCwnd(t, reg); n != 1 || v != 100 {
			t.Fatalf("gather %d: series=%d value=%v, want 1개 100 유지", i+1, n, v)
		}
	}
}

// TestTCPStateAggregator_WindowRotation은 tcpStateWindow 경과 후의 Observe가 누적치를 새 창으로
// 리셋하는지 검증한다. 회전이 없으면 직전 창의 min=100이 영구히 남는다.
func TestTCPStateAggregator_WindowRotation(t *testing.T) {
	agg := NewTCPStateAggregator()
	now := time.Unix(1_700_000_000, 0)
	agg.now = func() time.Time { return now }
	l := TCPStateLabels{Namespace: "ns", Pod: "p1", Node: "n1"}

	agg.Observe(l, 100, 5_000, 50)
	reg := prometheus.NewRegistry()
	reg.MustRegister(agg)
	if _, v := gatherMinCwnd(t, reg); v != 100 {
		t.Fatalf("회전 전 min_cwnd=%v want 100", v)
	}

	// 창(60s) 경과 후 더 큰 cwnd sample. 회전되면 200이 새 창의 min이다.
	now = now.Add(tcpStateWindow)
	agg.Observe(l, 200, 10_000, 70)
	if _, v := gatherMinCwnd(t, reg); v != 200 {
		t.Errorf("회전 후 min_cwnd=%v want 200 (새 창만 반영)", v)
	}
}

// TestTCPStateAggregator_IdlePruning은 sample이 tcpStateIdleTTL 이상 끊긴 entry를 Collect가
// 삭제하는지 검증한다. 비파괴 전환으로 종전 파괴적 리셋의 죽은 pod 정리를 대체하는 경로다.
func TestTCPStateAggregator_IdlePruning(t *testing.T) {
	agg := NewTCPStateAggregator()
	now := time.Unix(1_700_000_000, 0)
	agg.now = func() time.Time { return now }
	l := TCPStateLabels{Namespace: "ns", Pod: "p1", Node: "n1"}

	agg.Observe(l, 100, 5_000, 50)
	reg := prometheus.NewRegistry()
	reg.MustRegister(agg)
	if n, _ := gatherMinCwnd(t, reg); n != 1 {
		t.Fatalf("프루닝 전 series=%d want 1", n)
	}

	now = now.Add(tcpStateIdleTTL)
	if n, _ := gatherMinCwnd(t, reg); n != 0 {
		t.Errorf("idle TTL 경과 후 series=%d want 0 (프루닝)", n)
	}
}

// TestTCPStateAggregator_FiltersInvalidAndSentinel 은 cwnd/srtt=0 또는 ssthresh=
// TCP_INFINITE_SSTHRESH 가 min/max 집계에서 제외되어 emit 결과를 오염시키지 않는지 검증한다.
func TestTCPStateAggregator_FiltersInvalidAndSentinel(t *testing.T) {
	agg := NewTCPStateAggregator()
	l := TCPStateLabels{Namespace: "ns", Pod: "p1", Node: "n1"}

	// cwnd=0 / srtt=0 / ssthresh=TCP_INFINITE_SSTHRESH (0x7FFFFFFF) 만 들어온 케이스. emit 자체가
	// 없어야 한다.
	agg.Observe(l, 0, 0, 0x7FFFFFFF)

	reg := prometheus.NewRegistry()
	reg.MustRegister(agg)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if len(mf.Metric) > 0 {
			t.Errorf("invalid 만 들어온 Pod 가 %s emit 됨: %v", mf.GetName(), mf.Metric)
		}
	}
}

// TestTCPStateAggregator_SkipsEmptyLabels 는 namespace 또는 pod 가 비어 있으면 (수신 Pod 식별
// 실패) Observe 가 무시되어 cardinality 가 알려진 Pod 셋으로만 한정되는지 검증한다.
func TestTCPStateAggregator_SkipsEmptyLabels(t *testing.T) {
	agg := NewTCPStateAggregator()

	agg.Observe(TCPStateLabels{Namespace: "", Pod: "p1", Node: "n1"}, 100, 5_000, 50)
	agg.Observe(TCPStateLabels{Namespace: "ns", Pod: "", Node: "n1"}, 100, 5_000, 50)

	reg := prometheus.NewRegistry()
	reg.MustRegister(agg)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if len(mf.Metric) > 0 {
			t.Errorf("빈 라벨 sample 이 %s 로 emit 됨", mf.GetName())
		}
	}
}
