package metrics

import (
	"strconv"

	"netobs/internal/types"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	eventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_events_total",
			Help: "Total custom eBPF events by stage",
		},
		[]string{"stage"},
	)

	latencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_stage_latency_seconds",
			Help:    "Latency of selected kernel stages",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"stage"},
	)

	dropTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_drop_total",
			Help: "Drop events by kernel reason code",
		},
		[]string{"reason"},
	)
)

func Register(reg prometheus.Registerer) {
	reg.MustRegister(eventsTotal, latencySeconds, dropTotal)
}

func Record(ev types.Event) {
	stage := types.StageName(ev.Stage)
	eventsTotal.WithLabelValues(stage).Inc()

	switch ev.Stage {
	case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ:
		latencySeconds.WithLabelValues(stage).Observe(float64(ev.LatencyUs) / 1_000_000.0)
	case types.StageDrop:
		dropTotal.WithLabelValues(strconv.FormatUint(uint64(ev.Reason), 10)).Inc()
	}
}
