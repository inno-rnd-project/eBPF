package metrics

import (
	"math"
	"testing"

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

// TestTCPStateAggregator_ResetOnCollect 는 Collect 호출 후 누적치가 reset 되어 다음 scrape 가
// 직전 결과를 끌고 오지 않는지 검증한다. 동일 Pod 의 후속 sample 만 다음 window 에 반영되어야 한다.
func TestTCPStateAggregator_ResetOnCollect(t *testing.T) {
	agg := NewTCPStateAggregator()
	l := TCPStateLabels{Namespace: "ns", Pod: "p1", Node: "n1"}

	agg.Observe(l, 100, 5_000, 50)
	// 1 차 scrape 후 reset.
	reg := prometheus.NewRegistry()
	reg.MustRegister(agg)
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("first gather: %v", err)
	}

	// 2 차 sample 은 sample 1 보다 큰 cwnd. reset 안 되면 100 이 그대로 min 으로 남는다.
	agg.Observe(l, 200, 10_000, 70)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("second gather: %v", err)
	}
	got := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			got[mf.GetName()] = m.GetGauge().GetValue()
		}
	}
	if got["netobs_tcp_state_min_cwnd"] != 200 {
		t.Errorf("min_cwnd=%v want 200 (reset 후 신규 sample 만 반영)", got["netobs_tcp_state_min_cwnd"])
	}
}

// TestTCPStateAggregator_FiltersInvalidAndSentinel 은 cwnd/srtt=0 또는 ssthresh=
// TCP_INFINITE_SSTHRESH 가 min/max 집계에서 제외되어 emit 결과를 오염시키지 않는지 검증한다.
func TestTCPStateAggregator_FiltersInvalidAndSentinel(t *testing.T) {
	agg := NewTCPStateAggregator()
	l := TCPStateLabels{Namespace: "ns", Pod: "p1", Node: "n1"}

	// cwnd=0 / srtt=0 / ssthresh=infinite 만 들어온 케이스. emit 자체가 없어야 한다.
	agg.Observe(l, 0, 0, math.MaxUint32)

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
