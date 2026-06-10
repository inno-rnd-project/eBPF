package registry

// registerCorrelation 은 correlation 그룹의 1 종 mapping 을 등록한다. CorrelationStrongNoisyNeighbor
// 의 alert 라벨에 victim 과 suspect 가 모두 노출되어 있어 외부 Sources 조회 없이도 RCASummary
// 가 정확히 채워진다.
func registerCorrelation(r *Registry) {
	r.register("CorrelationStrongNoisyNeighbor", mapCorrelationStrongNoisyNeighbor)
}

// mapCorrelationStrongNoisyNeighbor 는 alert 라벨의 victim / suspect / resource_dimension 을
// 그대로 RCASummary 에 옮긴다. #122 의 multi-source cross-reference 산출 을 위해 victim 매칭
// noisy neighbor 와 drop flow Top-N 과 node 단위 GPU signal 을 추가 조회 후 EvaluateConfidence
// 의 결과 를 ConfidenceScore 필드 에 채운다. correlation alert 가 이미 victim/suspect 라벨 을
// 노출 하므로 neighbor lookup 은 cross-validation 목적 이며 RCASummary 의 TopSuspect 는 alert
// 라벨 의 suspect 를 그대로 유지 한다.
func mapCorrelationStrongNoisyNeighbor(labels map[string]string, sources Sources) RCASummary {
	victimNS := labelOr(labels, "victim_namespace", "")
	victimPod := labelOr(labels, "victim_pod", "")
	suspectNS := labelOr(labels, "suspect_namespace", "")
	suspectPod := labelOr(labels, "suspect_pod", "")
	dimension := labelOr(labels, "resource_dimension", "unknown")
	node := labelOr(labels, "node", "")

	summary := RCASummary{
		DominantDimension: dimension,
		TopSuspect:        formatPod(suspectNS, suspectPod),
		EvidenceMetrics: []string{
			"correlation_noisy_neighbor_score",
			"correlation_noisy_neighbor_lag_seconds",
		},
	}

	if sources != nil {
		var neighbors []NeighborInfo
		var dropFlows []DropFlowInfo
		if victimNS != "" && victimPod != "" {
			neighbors = sources.TopNeighbors(victimNS, victimPod)
		}
		if victimNS != "" {
			dropFlows = sources.TopDropFlows(victimNS)
		}
		gpuSignal := 0.0
		if node != "" {
			gpuSignal = sources.GPUSignal(node)
		}
		summary.ConfidenceScore = sources.EvaluateConfidence(neighbors, dropFlows, gpuSignal)
	}
	return summary
}
