package metrics

import (
	"strconv"

	"netobs/internal/types"

	"github.com/prometheus/client_golang/prometheus"
)

// podMetricsEnabled은 netobs_pod_stage_* 메트릭을 실제로 기록할지 결정함
// 클러스터에서 src_pod/src_pod_uid 라벨로 인한 Prometheus 카디널리티
// 폭증을 막기 위한 escape hatch로, 기본값은 true(기록)임
// startup 시점에 SetPodMetricsEnabled로만 설정되며 그 이후 읽기 전용으로 쓰임
var podMetricsEnabled = true

// SetPodMetricsEnabled은 pod-instance 레벨 메트릭 기록 여부를 전환하며,
// 반드시 Record가 호출되기 전 (main startup 단계)에 호출되어야 함
func SetPodMetricsEnabled(v bool) {
	podMetricsEnabled = v
}

var (
	legacyEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_events_total",
			Help: "Total custom eBPF events by stage",
		},
		[]string{"stage"},
	)

	legacyLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_stage_latency_seconds",
			Help:    "Latency of selected kernel stages",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"stage"},
	)

	legacyDropTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_drop_total",
			Help: "Drop events by kernel reason code",
		},
		[]string{"reason"},
	)

	stageEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_stage_events_labeled_total",
			Help: "Enriched eBPF events by stage, node, workload, and traffic scope",
		},
		[]string{"stage", "node", "src_namespace", "src_workload", "traffic_scope", "direction"},
	)

	stageLatencyLabeled = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_stage_latency_labeled_seconds",
			Help:    "Enriched latency by stage, node, workload, and traffic scope",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"stage", "node", "src_namespace", "src_workload", "traffic_scope", "direction"},
	)

	dropEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_drop_events_labeled_total",
			Help: "Enriched drop events with human-readable drop reason and category",
		},
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction", "drop_reason", "drop_category"},
	)

	retransEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_retrans_events_labeled_total",
			Help: "Enriched retrans events by node, workload, and traffic scope",
		},
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction"},
	)

	podStageEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_pod_stage_events_labeled_total",
			Help: "Enriched eBPF events by stage and source pod instance",
		},
		[]string{"stage", "node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction"},
	)

	podStageLatencyLabeled = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_pod_stage_latency_labeled_seconds",
			Help:    "Enriched latency by stage and source pod instance",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"stage", "node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction"},
	)
)

func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		legacyEventsTotal,
		legacyLatencySeconds,
		legacyDropTotal,
		stageEventsLabeled,
		stageLatencyLabeled,
		dropEventsLabeled,
		retransEventsLabeled,
		podStageEventsLabeled,
		podStageLatencyLabeled,
	)
}

func label(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

func podNameLabel(p types.PodIdentity) string {
	if p.PodName != "" {
		return p.PodName
	}
	return "unknown"
}

func podUIDLabel(p types.PodIdentity) string {
	if p.PodUID != "" {
		return p.PodUID
	}
	return "unknown"
}

func Record(ev types.EnrichedEvent) {
	stage := label(ev.Stage)

	legacyEventsTotal.WithLabelValues(stage).Inc()

	common := []string{
		stage,
		label(ev.ObservedNodeLabel()),
		label(ev.SourceNamespaceLabel()),
		label(ev.SourceWorkloadLabel()),
		label(ev.TrafficScope),
		label(ev.Direction),
	}

	stageEventsLabeled.WithLabelValues(common...).Inc()

	if podMetricsEnabled && ev.Src.IsPod() {
		podCommon := []string{
			stage,
			label(ev.ObservedNodeLabel()),
			label(ev.SourceNamespaceLabel()),
			label(podNameLabel(ev.Src)),
			label(podUIDLabel(ev.Src)),
			label(ev.TrafficScope),
			label(ev.Direction),
		}
		podStageEventsLabeled.WithLabelValues(podCommon...).Inc()

		switch ev.Raw.Stage {
		case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ:
			latencySec := float64(ev.Raw.LatencyUs) / 1_000_000.0
			podStageLatencyLabeled.WithLabelValues(podCommon...).Observe(latencySec)
		}
	}

	switch ev.Raw.Stage {
	case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ:
		latencySec := float64(ev.Raw.LatencyUs) / 1_000_000.0
		legacyLatencySeconds.WithLabelValues(stage).Observe(latencySec)
		stageLatencyLabeled.WithLabelValues(common...).Observe(latencySec)

	case types.StageDrop:
		legacyDropTotal.WithLabelValues(strconv.FormatUint(uint64(ev.Raw.Reason), 10)).Inc()
		dropEventsLabeled.WithLabelValues(
			label(ev.ObservedNodeLabel()),
			label(ev.SourceNamespaceLabel()),
			label(ev.SourceWorkloadLabel()),
			label(ev.TrafficScope),
			label(ev.Direction),
			label(ev.DropReasonName),
			label(ev.DropCategory),
		).Inc()

	case types.StageRetrans:
		retransEventsLabeled.WithLabelValues(
			label(ev.ObservedNodeLabel()),
			label(ev.SourceNamespaceLabel()),
			label(ev.SourceWorkloadLabel()),
			label(ev.TrafficScope),
			label(ev.Direction),
		).Inc()
	}
}
