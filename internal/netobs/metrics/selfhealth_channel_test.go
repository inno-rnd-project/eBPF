package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"netobs/internal/netobs/types"
)

// TestRegisterEventChannelDepth_ReflectsChannelLen 은 GaugeFunc 가 스크레이프 시점의 채널 적체를
// 그대로 반영함을 단정한다 (#413).
func TestRegisterEventChannelDepth_ReflectsChannelLen(t *testing.T) {
	reg := prometheus.NewRegistry()
	ch := make(chan types.Event, 8)
	RegisterEventChannelDepth(reg, ch, cap(ch))

	ch <- types.Event{}
	ch <- types.Event{}
	ch <- types.Event{}

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == "netobs_event_channel_depth" {
			got = mf
		}
	}
	if got == nil {
		t.Fatal("netobs_event_channel_depth 미등록")
	}
	if v := got.GetMetric()[0].GetGauge().GetValue(); v != 3 {
		t.Errorf("depth=%v want 3", v)
	}
	labels := got.GetMetric()[0].GetLabel()
	if len(labels) != 1 || labels[0].GetName() != "capacity" || labels[0].GetValue() != "8" {
		t.Errorf("capacity 라벨 불일치: %v", labels)
	}
}

// TestObserveEventProcessing_Accumulates 는 히스토그램 count 증가를 단정한다.
func TestObserveEventProcessing_Accumulates(t *testing.T) {
	before := histogramCount(t)
	ObserveEventProcessing(5 * time.Microsecond)
	after := histogramCount(t)
	if after != before+1 {
		t.Errorf("count %d -> %d, want +1", before, after)
	}
}

func histogramCount(t *testing.T) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := eventProcessingSeconds.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("write: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}
