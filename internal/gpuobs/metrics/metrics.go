package metrics

import "github.com/prometheus/client_golang/prometheus"

// AgentVersion은 에이전트의 버전 문자열이며, Phase 4 릴리스에서 ldflags로 치환된다.
// Phase 1에서는 "dev" 고정 문자열을 쓴다.
const AgentVersion = "dev"

var agentInfo = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Name:        "gpuobs_agent_info",
		Help:        "Static information about the gpuobs agent, value is always 1",
		ConstLabels: prometheus.Labels{"version": AgentVersion},
	},
)

// Register는 gpuobs 지표를 주어진 Prometheus Registerer에 등록한다.
// Phase 1에서는 agent_info 하나만 등록되며 값은 항상 1이다.
func Register(reg prometheus.Registerer) {
	agentInfo.Set(1)
	reg.MustRegister(agentInfo)
}
