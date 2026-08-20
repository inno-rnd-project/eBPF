package registry

import (
	"context"

	"fmt"
)

// registerNetobs 는 netobs 그룹의 1 종 mapping 을 등록한다. NetObsHighStageLatencyP99 와
// NetObsHighDropRate 는 victim 식별 라벨 (src_pod 또는 dst_pod) 이 없어 RCA 대상에서 제외했고,
// NetObsDropBurst 만 5-tuple 라벨 셋을 활용해 primary_drop_flow 를 채운다.
func registerNetobs(r *Registry) {
	r.register("NetObsDropBurst", mapNetObsDropBurst)
}

// mapNetObsDropBurst 는 alert 라벨의 5-tuple 을 primary_drop_flow 로 직렬화하고 dominant_dimension
// 을 network 로 두어 RCASummary 를 만든다. #122 의 multi-source cross-reference 산출 을 위해 victim
// 매칭 noisy neighbor 와 drop flow Top-N 과 node 단위 GPU signal 을 동시 조회 후 EvaluateConfidence
// 의 결과 를 ConfidenceScore 필드 에 채운다.
func mapNetObsDropBurst(ctx context.Context, labels map[string]string, sources Sources) RCASummary {
	srcNS := labelOr(labels, "src_namespace", "")
	srcPod := labelOr(labels, "src_pod", "")
	dstIP := labelOr(labels, "dst_ip", "")
	dstPort := labelOr(labels, "dst_port", "")
	proto := labelOr(labels, "protocol", "")
	reason := labelOr(labels, "drop_reason", "")

	summary := RCASummary{
		DominantDimension: "network",
		TopSuspect:        formatPod(srcNS, srcPod),
		PrimaryDropFlow:   fmt.Sprintf("%s -> %s:%s proto=%s reason=%s", formatPod(srcNS, srcPod), dstIP, dstPort, proto, reason),
		EvidenceMetrics: []string{
			"netobs_drop_burst:rate1m",
			"netobs_drop_events_flow_total",
		},
	}

	if sources != nil {
		var neighbors []NeighborInfo
		var dropFlows []DropFlowInfo
		if srcNS != "" && srcPod != "" {
			neighbors = sources.TopNeighbors(ctx, srcNS, srcPod)
		}
		if srcNS != "" {
			dropFlows = sources.TopDropFlows(ctx, srcNS)
		}
		// netobs_drop_burst:rate1m 은 5-tuple 단위 집계라 alert 라벨에 node 가 없어 GPU signal
		// 조회 분기가 영구 미실행이었다 (#447 죽은 코드 제거). GPU 신호는 0 으로 고정된다.
		summary.ConfidenceScore = sources.EvaluateConfidence(neighbors, dropFlows, 0)
	}
	return summary
}
