package correlation

import (
	"sort"
	"strings"
)

// ResourceDimension 은 noisy neighbor 메트릭의 resource_dimension 라벨에 들어가는 자원 종류다.
// 본 이슈 #51 의 메트릭 schema 가 cpu / memory / network / gpu 네 가지를 요구한다. latency 는
// dimension 이 아니라 victim 식별 기준이라 별도 상수로 두지 않는다 (latency 페어만 noisy neighbor
// 모델에 부합한다). 이슈 원안의 disk_io 는 본 시리즈가 disk 차원 cause score 를 도입하지 않아
// 정의 불가하므로 본 패키지에서 제외한다.
type ResourceDimension string

const (
	DimensionCPU     ResourceDimension = "cpu"
	DimensionMemory  ResourceDimension = "memory"
	DimensionNetwork ResourceDimension = "network"
	DimensionGPU     ResourceDimension = "gpu"
	// DimensionUnknown 은 metric 문자열에서 dimension 을 식별할 수 없을 때 반환된다. SelectTopN 은
	// 본 값을 가진 결과를 결과에서 제외해 카디널리티 폭발을 차단한다.
	DimensionUnknown ResourceDimension = ""
)

// PodIdentity 는 noisy neighbor 결과에서 victim 또는 suspect 한쪽을 가리키는 최소 식별자다. PairKey
// 가 두 Pod 를 한 struct 에 묶는 데 비해 NoisyNeighbor 는 victim 과 suspect 를 각각 별도 struct 로
// 분리해 운영자가 결과 해석 시 양쪽을 헷갈리지 않게 한다.
type PodIdentity struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	PodUID    string `json:"pod_uid"`
}

// NoisyNeighbor 는 한 victim 의 한 dimension 에서 채택된 단일 suspect 의 페어 정보다. exporter 가
// 본 struct 를 correlation_noisy_neighbor_score 와 correlation_noisy_neighbor_lag_seconds 두 메트릭
// 으로 변환한다.
type NoisyNeighbor struct {
	Victim        PodIdentity       `json:"victim"`
	VictimMetric  string            `json:"victim_metric"`
	Suspect       PodIdentity       `json:"suspect"`
	SuspectMetric string            `json:"suspect_metric"`
	Dimension     ResourceDimension `json:"dimension"`
	// Rank 는 (victim, dimension) 그룹 내 max_abs_value 내림차순 1-based 순위다. 1 이 가장 강한 상관.
	Rank        int     `json:"rank"`
	Score       float64 `json:"score"`
	LagSteps    int     `json:"lag_steps"`
	SampleCount int     `json:"sample_count"`
	// #69 Granger causality 결과. PValue 는 src 가 dst 를 Granger-cause 하는지의 통계적 유의성.
	// GrangerOK=false 면 표본 부족 또는 행렬 singular 로 산정이 자연 skip 되었다.
	PValue    float64 `json:"p_value"`
	GrangerOK bool    `json:"granger_ok"`
}

// classifyDimension 은 query 문자열에서 ResourceDimension 을 결정한다. 매칭 우선순위는 더 구체적인
// 키워드 (gpu) 가 더 일반적인 키워드 (memory) 보다 먼저 잡히도록 둔다. 예: pod:gpu_memory_utilization_ratio:5m
// 는 gpu 가 memory 보다 먼저 잡혀 DimensionGPU 로 분류된다.
//
// host_compute_stall_score 는 host CPU 가 GPU compute 를 stall 시키는 score 라 cpu 차원에 두며
// compute 키워드를 cpu case 에 포함한다.
func classifyDimension(metric string) ResourceDimension {
	switch {
	case strings.Contains(metric, "gpu"):
		return DimensionGPU
	case strings.Contains(metric, "network"):
		return DimensionNetwork
	case strings.Contains(metric, "memory"):
		return DimensionMemory
	case strings.Contains(metric, "cpu") || strings.Contains(metric, "compute"):
		return DimensionCPU
	}
	return DimensionUnknown
}

// isLatencyMetric 은 query 가 victim latency 메트릭인지 식별한다. SelectTopN 은 페어 정확히 한쪽이
// latency 인 결과만 채택해 noisy neighbor 모델 (suspect 자원 압박 → victim latency 손해) 에 부합한
// 케이스만 노출한다.
func isLatencyMetric(metric string) bool {
	return strings.Contains(metric, "latency")
}

// SelectTopN 은 Correlate 결과에서 noisy neighbor 페어를 추출한다. 다음 규칙을 단정적으로 적용한다.
//
//   - 페어 정확히 한쪽이 latency 메트릭이고 반대쪽이 non-latency cause score 인 페어만 채택한다.
//     양쪽 모두 latency 거나 양쪽 모두 non-latency 인 페어는 noisy neighbor 모델에 부합하지 않아
//     제외한다.
//   - 채택된 페어 중 Src 가 non-latency suspect, Dst 가 latency victim 인 방향만 사용한다. 반대
//     방향 (EnumeratePairs 가 만드는 (Y,X)) 은 같은 (victim, suspect) 가 두 번 등장하지 않도록
//     자동 dedup 의미로 제외한다. 결과적으로 채택된 lag 부호는 "suspect 가 victim 을 N step 앞선다"
//     인과 방향으로 자연스럽게 정렬된다.
//   - Status 가 StatusOK 또는 StatusPartial 인 결과만 채택한다. SkippedConstant / SkippedLowSamples
//     는 의미 있는 score 가 없어 제외한다.
//   - suspect 메트릭에서 dimension 을 분류해 DimensionUnknown 이면 제외한다 (카디널리티 가드).
//   - (victim_namespace, victim_pod, victim_pod_uid, dimension) 그룹별 max_abs_value 내림차순으로
//     정렬해 상위 topN 개에 rank 1..topN 부여한다. 동률은 (suspect_namespace, suspect_pod,
//     suspect_pod_uid) 라벨 lexicographic 순서로 결정한다.
//
// 결과는 (victim_namespace, victim_pod, dimension, rank) 순으로 정렬된 슬라이스다. 동일 cycle 의
// 재현성을 보장한다. topN <= 0 이면 nil 을 반환한다.
func SelectTopN(results []CorrelationResult, topN int) []NoisyNeighbor {
	if topN <= 0 {
		return nil
	}

	type candidate struct {
		victim        PodIdentity
		victimMetric  string
		suspect       PodIdentity
		suspectMetric string
		dimension     ResourceDimension
		score         float64
		lag           int
		samples       int
		// #69 Granger causality 결과. GrangerOK=false 면 pvalue=0 으로 자연 skip 되며 dedup 의 max
		// score 비교에서 동률일 경우 GrangerOK=true 한쪽이 우선 채택된다.
		pvalue    float64
		grangerOK bool
	}

	candidates := make([]candidate, 0, len(results))
	for _, r := range results {
		if r.Status != StatusOK && r.Status != StatusPartial {
			continue
		}
		srcLatency := isLatencyMetric(r.Pair.SrcMetric)
		dstLatency := isLatencyMetric(r.Pair.DstMetric)
		if srcLatency == dstLatency {
			continue
		}
		if srcLatency {
			continue
		}
		dim := classifyDimension(r.Pair.SrcMetric)
		if dim == DimensionUnknown {
			continue
		}
		// same-pod cross-metric pair 는 noisy neighbor 모델 (이웃 Pod 의 자원 압박) 에 부합하지
		// 않는다. EnumeratePairs 가 동일 Pod 의 두 다른 metric series 도 페어로 만들어 victim 의
		// 자기 자신이 suspect rank 1 을 차지할 수 있어 본 단계에서 명시 제외한다. PodUID 가 있으면
		// UID 기준이 가장 정확하고 둘 다 비어 있으면 namespace/pod 로 보수적 비교한다.
		if r.Pair.SrcPodUID != "" && r.Pair.DstPodUID != "" {
			if r.Pair.SrcPodUID == r.Pair.DstPodUID {
				continue
			}
		} else if r.Pair.SrcNamespace == r.Pair.DstNamespace && r.Pair.SrcPod == r.Pair.DstPod {
			continue
		}
		candidates = append(candidates, candidate{
			victim: PodIdentity{
				Namespace: r.Pair.DstNamespace,
				Pod:       r.Pair.DstPod,
				PodUID:    r.Pair.DstPodUID,
			},
			victimMetric: r.Pair.DstMetric,
			suspect: PodIdentity{
				Namespace: r.Pair.SrcNamespace,
				Pod:       r.Pair.SrcPod,
				PodUID:    r.Pair.SrcPodUID,
			},
			suspectMetric: r.Pair.SrcMetric,
			dimension:     dim,
			score:         r.MaxAbsValue,
			lag:           r.MaxAbsLag,
			samples:       r.SampleCount,
			pvalue:        r.PValue,
			grangerOK:     r.GrangerOK,
		})
	}

	// 한 suspect 가 같은 dimension 에 매핑되는 여러 metric (예: cpu_throttle_score 와
	// host_compute_stall_score 둘 다 cpu) 로 여러 candidate 를 만드는 경우가 있다. Top-N 이 metric
	// 수가 아닌 unique noisy-neighbor Pod 수를 세야 하므로 (victim, suspect, dimension) 단위로
	// max score 를 가진 candidate 하나만 채택해 dedup 한다.
	type pairKey struct {
		victimNamespace  string
		victimPod        string
		victimPodUID     string
		suspectNamespace string
		suspectPod       string
		suspectPodUID    string
		dimension        ResourceDimension
	}
	byPair := make(map[pairKey]candidate)
	for _, c := range candidates {
		k := pairKey{
			victimNamespace:  c.victim.Namespace,
			victimPod:        c.victim.Pod,
			victimPodUID:     c.victim.PodUID,
			suspectNamespace: c.suspect.Namespace,
			suspectPod:       c.suspect.Pod,
			suspectPodUID:    c.suspect.PodUID,
			dimension:        c.dimension,
		}
		if prev, ok := byPair[k]; !ok || c.score > prev.score {
			byPair[k] = c
		}
	}
	deduped := make([]candidate, 0, len(byPair))
	for _, c := range byPair {
		deduped = append(deduped, c)
	}

	type groupKey struct {
		namespace string
		pod       string
		podUID    string
		dimension ResourceDimension
	}
	groups := make(map[groupKey][]candidate)
	for _, c := range deduped {
		k := groupKey{c.victim.Namespace, c.victim.Pod, c.victim.PodUID, c.dimension}
		groups[k] = append(groups[k], c)
	}

	groupKeys := make([]groupKey, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		a, b := groupKeys[i], groupKeys[j]
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		if a.pod != b.pod {
			return a.pod < b.pod
		}
		if a.podUID != b.podUID {
			return a.podUID < b.podUID
		}
		return a.dimension < b.dimension
	})

	out := make([]NoisyNeighbor, 0)
	for _, gk := range groupKeys {
		cands := groups[gk]
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].score != cands[j].score {
				return cands[i].score > cands[j].score
			}
			if cands[i].suspect.Namespace != cands[j].suspect.Namespace {
				return cands[i].suspect.Namespace < cands[j].suspect.Namespace
			}
			if cands[i].suspect.Pod != cands[j].suspect.Pod {
				return cands[i].suspect.Pod < cands[j].suspect.Pod
			}
			return cands[i].suspect.PodUID < cands[j].suspect.PodUID
		})
		limit := topN
		if len(cands) < limit {
			limit = len(cands)
		}
		for i := 0; i < limit; i++ {
			c := cands[i]
			out = append(out, NoisyNeighbor{
				Victim:        c.victim,
				VictimMetric:  c.victimMetric,
				Suspect:       c.suspect,
				SuspectMetric: c.suspectMetric,
				Dimension:     c.dimension,
				Rank:          i + 1,
				Score:         c.score,
				LagSteps:      c.lag,
				SampleCount:   c.samples,
				PValue:        c.pvalue,
				GrangerOK:     c.grangerOK,
			})
		}
	}
	return out
}
