package registry

// registerGpuobs 는 gpuobs 그룹의 2 종 mapping 을 등록한다. GPUObsCudaStreamWaitHigh 는 src_pod
// 라벨로 victim 을 식별 가능하고, GPUObsThermalThrottleSustained 는 node + gpu_uuid 만 노출되어
// pod 단위 victim 이 없다.
func registerGpuobs(r *Registry) {
	r.register("GPUObsCudaStreamWaitHigh", mapGPUObsCudaStreamWaitHigh)
	r.register("GPUObsThermalThrottleSustained", mapGPUObsThermalThrottleSustained)
}

// mapGPUObsCudaStreamWaitHigh 는 victim Pod (src_namespace, src_pod) 의 noisy neighbor Top-N 을
// Sources 에서 가져와 [0] 을 top_suspect 로 채운다. cuStreamSync 지연은 GPU 자원 차원의 신호라
// dominant_dimension 을 gpu 로 두지만, neighbor 의 dimension 이 다른 경우 (예: cpu) 그 dimension
// 으로 overwrite 해 진짜 원인 차원을 노출한다. #122 의 multi-source cross-reference 산출 시 세
// source 의 raw 결과 를 모아 EvaluateConfidence 로 ConfidenceScore 를 채운다.
func mapGPUObsCudaStreamWaitHigh(labels map[string]string, sources Sources) RCASummary {
	srcNS := labelOr(labels, "src_namespace", "")
	srcPod := labelOr(labels, "src_pod", "")
	node := labelOr(labels, "node", "")

	summary := RCASummary{
		DominantDimension: "gpu",
		TopSuspect:        formatPod(srcNS, srcPod),
		EvidenceMetrics: []string{
			"gpuobs_cuda_stream_synchronize_seconds",
			"correlation_noisy_neighbor_score",
		},
	}

	if sources != nil {
		var neighbors []NeighborInfo
		var dropFlows []DropFlowInfo
		if srcNS != "" && srcPod != "" {
			neighbors = sources.TopNeighbors(srcNS, srcPod)
			if len(neighbors) > 0 {
				summary.TopSuspect = formatPod(neighbors[0].SuspectNamespace, neighbors[0].SuspectPod)
				if neighbors[0].Dimension != "" {
					summary.DominantDimension = neighbors[0].Dimension
				}
			}
		}
		if srcNS != "" {
			dropFlows = sources.TopDropFlows(srcNS)
		}
		gpuSignal := 0.0
		if node != "" {
			gpuSignal = sources.GPUSignal(node)
		}
		summary.ConfidenceScore = sources.EvaluateConfidence(neighbors, dropFlows, gpuSignal)
	}
	return summary
}

// mapGPUObsThermalThrottleSustained 는 node + gpu_uuid 라벨만 활용한다. victim Pod 식별이 불가
// 하므로 top_suspect 는 node 단위 식별자 ("node/<name>") 로 둔다. dimension 은 thermal 로 둬
// dashboard 가 throttle 신호임을 구분 가능하게 한다. #122 의 multi-source cross-reference 산출
// 시 GPU signal 만 활용 한다 (victim Pod 미식별 으로 correlation 과 netobs factor 는 자연 0).
func mapGPUObsThermalThrottleSustained(labels map[string]string, sources Sources) RCASummary {
	node := labelOr(labels, "node", "")
	gpuUUID := labelOr(labels, "gpu_uuid", "")

	suspect := ""
	if node != "" {
		suspect = "node/" + node
		if gpuUUID != "" {
			suspect += " gpu=" + gpuUUID
		}
	}

	summary := RCASummary{
		DominantDimension: "thermal",
		TopSuspect:        suspect,
		EvidenceMetrics: []string{
			"gpuobs_device_temperature_celsius",
			"gpuobs_device_throttle_active",
			"node:gpu_thermal_throttle_correlation:5m",
		},
	}

	if sources != nil {
		gpuSignal := 0.0
		if node != "" {
			gpuSignal = sources.GPUSignal(node)
		}
		summary.ConfidenceScore = sources.EvaluateConfidence(nil, nil, gpuSignal)
	}
	return summary
}
