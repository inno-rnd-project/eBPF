package registry

// registerCorrelation 은 correlation 그룹의 1 종 mapping 을 등록한다. CorrelationStrongNoisyNeighbor
// 의 alert 라벨에 victim 과 suspect 가 모두 노출되어 있어 외부 Sources 조회 없이도 RCASummary
// 가 정확히 채워진다.
func registerCorrelation(r *Registry) {
	r.register("CorrelationStrongNoisyNeighbor", mapCorrelationStrongNoisyNeighbor)
}

// mapCorrelationStrongNoisyNeighbor 는 alert 라벨의 victim / suspect / resource_dimension 을
// 그대로 RCASummary 에 옮긴다. Sources 호출이 없어 본 mapping 은 가장 빠른 응답 경로다.
func mapCorrelationStrongNoisyNeighbor(labels map[string]string, _ Sources) RCASummary {
	suspectNS := labelOr(labels, "suspect_namespace", "")
	suspectPod := labelOr(labels, "suspect_pod", "")
	dimension := labelOr(labels, "resource_dimension", "unknown")

	return RCASummary{
		DominantDimension: dimension,
		TopSuspect:        formatPod(suspectNS, suspectPod),
		EvidenceMetrics: []string{
			"correlation_noisy_neighbor_score",
			"correlation_noisy_neighbor_lag_seconds",
		},
	}
}
