package registry

import "fmt"

// registerNetobs 는 netobs 그룹의 1 종 mapping 을 등록한다. NetObsHighStageLatencyP99 와
// NetObsHighDropRate 는 victim 식별 라벨 (src_pod 또는 dst_pod) 이 없어 RCA 대상에서 제외했고,
// NetObsDropBurst 만 5-tuple 라벨 셋을 활용해 primary_drop_flow 를 채운다.
func registerNetobs(r *Registry) {
	r.register("NetObsDropBurst", mapNetObsDropBurst)
}

// mapNetObsDropBurst 는 alert 라벨의 5-tuple 을 primary_drop_flow 로 직렬화하고 dominant_dimension
// 을 network 로 두어 RCASummary 를 만든다. alert expr 이 이미 5-tuple 수준이라 Sources.TopDropFlows
// 가 돌려주는 추가 flow 는 본 alert 의 진단에 잉여라 호출하지 않는다.
func mapNetObsDropBurst(labels map[string]string, _ Sources) RCASummary {
	srcNS := labelOr(labels, "src_namespace", "")
	srcPod := labelOr(labels, "src_pod", "")
	dstIP := labelOr(labels, "dst_ip", "")
	dstPort := labelOr(labels, "dst_port", "")
	proto := labelOr(labels, "protocol", "")
	reason := labelOr(labels, "drop_reason", "")

	return RCASummary{
		DominantDimension: "network",
		TopSuspect:        formatPod(srcNS, srcPod),
		PrimaryDropFlow:   fmt.Sprintf("%s -> %s:%s proto=%s reason=%s", formatPod(srcNS, srcPod), dstIP, dstPort, proto, reason),
		EvidenceMetrics: []string{
			"netobs_drop_burst:rate1m",
			"netobs_drop_events_flow_total",
		},
	}
}
