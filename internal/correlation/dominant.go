package correlation

import (
	"cmp"
	"slices"
)

// DominantDimension 은 한 victim Pod 의 4 dimension 중 sum 정규화 weight 가 가장 큰 dimension 1 종
// 이다. correlation-exporter 의 Collector 가 본 슬라이스를 그대로 받아 단일 라벨 series 로 emit
// 한다.
type DominantDimension struct {
	Victim    PodIdentity
	Dimension ResourceDimension
	// Weight 는 dominant dimension 의 순수 정규화 weight (score / sum) 이다. tie-breaker offset 은
	// 본 필드에 가산되지 않아 항상 [0, 1] 범위에 머무르며 dashboard 와 alert 가 raw 정규화 값을
	// 그대로 본다.
	Weight float64
}

// dimensionOffset 은 dimension enum 사전순 tie-breaker 의 deterministic offset 이다. 사전순 가장 앞
// (cpu) 이 가장 큰 offset 을 받아 dimension score 동률 시 우선 채택된다. 1e-6 단위라 어떤 실 weight
// 값에도 미치지 못해 일반 케이스의 정렬을 변경하지 않는다.
var dimensionOffset = map[ResourceDimension]float64{
	DimensionCPU:     4e-6,
	DimensionGPU:     3e-6,
	DimensionMemory:  2e-6,
	DimensionNetwork: 1e-6,
}

// dominantDimensionEnum 은 산정 시 victim 별로 0 으로 시작해 dimension max score 를 누적할 때 순회
// 순서를 정한다. enum 사전순으로 deterministic.
var dominantDimensionEnum = []ResourceDimension{
	DimensionCPU,
	DimensionGPU,
	DimensionMemory,
	DimensionNetwork,
}

// ComputeDominantDimension 은 NoisyNeighbor snapshot 에서 victim 단위로 4 dimension 의 max score 를
// 집계한 뒤 sum 정규화 weight 를 산정해 dominant dimension 1 종을 채택한다. 4 dimension 합이 0 인
// victim 은 결과에서 제외되어 dashboard 시리즈가 빈 victim 으로 폭증하는 것을 막는다. 정확 동률 시
// dimensionOffset 으로 enum 사전순 가장 앞 dimension 이 채택된다.
func ComputeDominantDimension(neighbors []NoisyNeighbor) []DominantDimension {
	type victimKey struct {
		namespace string
		pod       string
		podUID    string
	}
	type victimAgg struct {
		victim PodIdentity
		scores map[ResourceDimension]float64
	}

	aggregated := make(map[victimKey]*victimAgg)
	for _, n := range neighbors {
		if n.Dimension == DimensionUnknown {
			continue
		}
		k := victimKey{n.Victim.Namespace, n.Victim.Pod, n.Victim.PodUID}
		a, ok := aggregated[k]
		if !ok {
			a = &victimAgg{victim: n.Victim, scores: make(map[ResourceDimension]float64)}
			aggregated[k] = a
		}
		if n.Score > a.scores[n.Dimension] {
			a.scores[n.Dimension] = n.Score
		}
	}

	keys := make([]victimKey, 0, len(aggregated))
	for k := range aggregated {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b victimKey) int {
		if c := cmp.Compare(a.namespace, b.namespace); c != 0 {
			return c
		}
		if c := cmp.Compare(a.pod, b.pod); c != 0 {
			return c
		}
		return cmp.Compare(a.podUID, b.podUID)
	})

	out := make([]DominantDimension, 0, len(keys))
	for _, k := range keys {
		a := aggregated[k]
		sum := 0.0
		for _, dim := range dominantDimensionEnum {
			sum += a.scores[dim]
		}
		if sum <= 0 {
			continue
		}
		var dominant ResourceDimension
		var dominantWeight float64
		var bestCompare float64
		first := true
		for _, dim := range dominantDimensionEnum {
			weight := a.scores[dim] / sum
			// 비교용 점수는 offset 을 가산해 정확 동률 시 enum 사전순 가장 앞이 채택되도록 한다.
			// 노출되는 Weight 자체는 offset 없는 raw 정규화 값을 그대로 둔다.
			compare := weight + dimensionOffset[dim]
			if first || compare > bestCompare {
				bestCompare = compare
				dominantWeight = weight
				dominant = dim
				first = false
			}
		}
		out = append(out, DominantDimension{
			Victim:    a.victim,
			Dimension: dominant,
			Weight:    dominantWeight,
		})
	}
	return out
}
