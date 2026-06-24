package registry

// registerIdle 은 gpu-idle 그룹의 7 종 mapping 을 등록한다. 앞 5 종은 src_namespace 와 src_pod 라벨로
// victim 을 식별 가능하고, cause 별로 dominant_dimension 이 alert 이름에 인코딩되어 있어 mapping 흐름이
// 동일하다 (cause 만 다름). 뒤 2 종 (dcgm_pcie_replay, nccl_collective_stall) 은 base score 가 cluster
// 단위라 alert 라벨에 src_namespace / src_pod / node 가 없어 idleMapping 이 victim / node 식별을 자연
// skip 하고 dimension 과 evidence 만 채운다 (#155).
func registerIdle(r *Registry) {
	r.register("GPUIdleWithPCIeSaturation", idleMapping("network", []string{
		"node:gpu_pcie_saturation_score:5m",
		"gpuobs_device_pcie_rx_bps",
		"gpuobs_device_pcie_tx_bps",
	}))
	r.register("GPUIdleWithNetworkPressure", idleMapping("network", []string{
		"pod:network_throughput_score:5m",
		"pod:network_retrans_score:5m",
	}))
	r.register("GPUIdleWithCPUThrottle", idleMapping("cpu", []string{
		"pod:cpu_throttle_score:5m",
	}))
	r.register("GPUIdleWithMemoryPressure", idleMapping("memory", []string{
		"pod:memory_pressure_score:5m",
	}))
	r.register("GPUIdleWithHostComputeStall", idleMapping("cpu", []string{
		"pod:host_compute_stall_score:5m",
		"gpuobs_cuda_kernel_launches_total",
	}))
	// dcgm_pcie_replay 와 nccl_collective_stall 은 GPU 도메인 hardware / collective 신호라 dimension
	// 을 gpu 로 둔다. base score 가 cluster 단위라 victim Pod 가 특정되지 않으므로 evidence 메트릭으로
	// 운영자가 datacenter GPU 환경에서 직접 추적한다.
	r.register("GPUIdleWithDCGMPCIeReplay", idleMapping("gpu", []string{
		"cluster:dcgm_pcie_replay_score:5m",
		"DCGM_FI_DEV_PCIE_REPLAY_COUNTER",
	}))
	r.register("GPUIdleWithNCCLCollectiveStall", idleMapping("gpu", []string{
		"cluster:nccl_collective_stall_score:5m",
		"gpuobs_nccl_collective_duration_seconds",
	}))
}

// idleMapping 은 GPUIdleWith* 7 종 alert 의 공통 흐름을 closure 로 캡슐화한다. dimension 은
// cause 차원이며 evidence 는 cause 별로 dashboard 에서 운영자가 즉시 참조 가능한 메트릭 키를
// 채운다. victim Pod 식별이 가능하면 (cluster 단위 cause 는 src 라벨이 없어 skip) noisy neighbor
// Top-N 의 [0] 으로 top_suspect 를 갱신한다. #122 의 multi-source cross-reference 산출 시 세 source 의 raw 결과
// 를 모아 EvaluateConfidence 로 ConfidenceScore 를 채운다.
func idleMapping(dimension string, evidence []string) Mapping {
	return func(labels map[string]string, sources Sources) RCASummary {
		srcNS := labelOr(labels, "src_namespace", "")
		srcPod := labelOr(labels, "src_pod", "")
		node := labelOr(labels, "node", "")

		summary := RCASummary{
			DominantDimension: dimension,
			TopSuspect:        formatPod(srcNS, srcPod),
			EvidenceMetrics:   append([]string(nil), evidence...),
		}
		if sources != nil {
			var neighbors []NeighborInfo
			var dropFlows []DropFlowInfo
			if srcNS != "" && srcPod != "" {
				neighbors = sources.TopNeighbors(srcNS, srcPod)
				if len(neighbors) > 0 {
					summary.TopSuspect = formatPod(neighbors[0].SuspectNamespace, neighbors[0].SuspectPod)
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
}
