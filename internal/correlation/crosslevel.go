package correlation

import "sort"

// #149 cross-granularity (cross-level) layer 의 라이브러리 entry point 다. pod-level (noisy neighbor)
// 과 node-level (cross-node) 분석이 의도적으로 분리되어 한 node 의 압박이 그 위의 특정 pod 에 주는
// 영향이나, 한 pod 가 자신이 속한 node 전체에 주는 영향을 잇는 경로가 없었다. 본 layer 는 동일 node
// 안에서 node 입도 시계열과 pod 입도 시계열을 잇는 페어를 enumerate 해 두 방향의 영향을 산정한다.
//
// 입력은 모두 기존 layer 가 이미 fetch 하는 시계열을 재사용한다. node 압박 / node latency 는
// CrossNodeMetrics, pod 압박 / pod latency 는 DefaultMetrics 에 존재하므로 본 layer 는 새 recording
// rule 없이 node 라벨로 동일 node 를 매칭해 cross-granularity 페어만 새로 만든다. 서로 다른 node 의
// node 와 pod 를 잇는 분석은 이슈 비목표라 same-node 페어만 생성한다.

// CrossLevelDirection 은 cross-level 페어의 방향이다. 두 방향 모두 suspect (압박, src) 가 victim
// (latency, dst) 를 예측하는 noisy neighbor 모델을 따르며, 입도 조합으로 방향을 구분한다.
type CrossLevelDirection string

const (
	// DirectionNodeToPod 은 node 압박 (suspect) 이 동일 node 의 pod latency (victim) 에 주는 영향이다.
	DirectionNodeToPod CrossLevelDirection = "node_to_pod"
	// DirectionPodToNode 은 pod 압박 (suspect) 이 자신이 속한 node 의 latency (victim) 에 주는 영향이다.
	DirectionPodToNode CrossLevelDirection = "pod_to_node"
)

// CrossLevelPairKey 는 cross-level 분석의 페어 식별자다. Node 는 두 시계열이 공유하는 동일 node 이며
// 방향과 무관하게 pod 측 식별자 (PodNamespace / Pod / PodUID) 를 함께 보존한다. SrcMetric 은 항상
// suspect (압박) 메트릭, DstMetric 은 항상 victim (latency) 메트릭이라 dimension 은 SrcMetric 에서
// 분류된다. DirectionNodeToPod 면 src 가 node, dst 가 pod 이고 DirectionPodToNode 면 그 반대다.
type CrossLevelPairKey struct {
	Node         string              `json:"node"`
	Direction    CrossLevelDirection `json:"direction"`
	PodNamespace string              `json:"pod_namespace"`
	Pod          string              `json:"pod"`
	PodUID       string              `json:"pod_uid"`
	SrcMetric    string              `json:"src_metric"`
	DstMetric    string              `json:"dst_metric"`
}

// CrossLevelPair 는 CrossLevelPairKey 와 두 시계열의 참조다. PearsonWithLag 입력으로 그대로 전달
// 가능하다.
type CrossLevelPair struct {
	Key CrossLevelPairKey
	Src TimeSeries
	Dst TimeSeries
}

// CrossLevel 은 한 (node, direction, dimension) 그룹에서 채택된 단일 pod 의 페어 정보다. exporter 가
// 본 struct 를 correlation_cross_level_score{node, pod_namespace, pod, direction, dimension} 메트릭으로
// 변환한다.
type CrossLevel struct {
	Node          string              `json:"node"`
	Direction     CrossLevelDirection `json:"direction"`
	PodNamespace  string              `json:"pod_namespace"`
	Pod           string              `json:"pod"`
	PodUID        string              `json:"pod_uid"`
	SuspectMetric string              `json:"suspect_metric"`
	VictimMetric  string              `json:"victim_metric"`
	Dimension     ResourceDimension   `json:"dimension"`
	Rank          int                 `json:"rank"`
	Score         float64             `json:"score"`
	LagSteps      int                 `json:"lag_steps"`
	SampleCount   int                 `json:"sample_count"`
	PValue        float64             `json:"p_value"`
	GrangerOK     bool                `json:"granger_ok"`
}

// EnumerateCrossLevelPairs 는 입력 LabeledSeries 슬라이스에서 동일 node 의 cross-granularity 페어를
// 생성한다. 정책은 다음과 같다.
//
//   - 입력 시계열을 라벨 schema 로 네 부류로 분류한다. node 압박 (node 라벨만, non-latency), node
//     latency (node 라벨만, latency), pod 압박 (node + src_namespace + src_pod, non-latency), pod
//     latency (node + src_namespace + src_pod, latency).
//   - DirectionNodeToPod: 각 node 의 node 압박 (src) × 동일 node 의 pod latency (dst).
//   - DirectionPodToNode: 각 node 의 pod 압박 (src) × 동일 node 의 node latency (dst).
//   - allowNamespaces 가 비어 있지 않으면 pod 측 src_namespace 가 목록에 든 페어만 생성해 카디널리티를
//     통제한다 (allow-list). 비어 있으면 모든 namespace 를 허용하고 호출자의 max-pairs 캡이 backstop 이 된다.
//
// node 키와 메트릭 / pod 키 정렬로 출력 순서가 결정적이라 max-pairs 트림과 단위 테스트가 안정적이다.
func EnumerateCrossLevelPairs(items []LabeledSeries, allowNamespaces []string) []CrossLevelPair {
	allow := make(map[string]struct{}, len(allowNamespaces))
	for _, ns := range allowNamespaces {
		allow[ns] = struct{}{}
	}
	namespaceAllowed := func(ns string) bool {
		if len(allow) == 0 {
			return true
		}
		_, ok := allow[ns]
		return ok
	}

	type podSeries struct {
		namespace string
		pod       string
		podUID    string
		metric    string
		series    TimeSeries
	}
	nodePressure := make(map[string][]LabeledSeries)
	nodeLatency := make(map[string][]LabeledSeries)
	podPressure := make(map[string][]podSeries)
	podLatency := make(map[string][]podSeries)
	// nodeSeen 은 분류 맵에 실제로 합류한 시계열의 node 키 집합이다. 분류 맵 4개를 다시 순회하지
	// 않도록 items 순회 중에 함께 채운다. allow-list 에 막힌 pod 는 continue 가 본 기록 앞에 있어
	// node 가 등록되지 않으므로 기존과 동일하게 페어에서 제외된다.
	nodeSeen := make(map[string]struct{})

	for _, item := range items {
		node := item.Series.Labels["node"]
		if node == "" {
			continue
		}
		ns := item.Series.Labels["src_namespace"]
		pod := item.Series.Labels["src_pod"]
		// #150 cross-level 은 victim 을 latency 단일로 유지한다. latency victim 과 victim 이 아닌 suspect
		// 만 분류하고, throughput / error victim (#150) 과 gpu victim (#174) 은 suspect 도 victim 도 아니라
		// 본 layer 에서 제외해 pod-level 비-latency victim 이 pod-suspect 로 오분류되는 것을 막는다.
		signal := classifyVictimSignal(item.Metric)
		isLatencyVictim := signal == SignalLatency
		isSuspect := signal == SignalNone
		if !isLatencyVictim && !isSuspect {
			continue
		}
		switch {
		case ns == "" && pod == "":
			// node-level 시계열.
			if isLatencyVictim {
				nodeLatency[node] = append(nodeLatency[node], item)
			} else {
				nodePressure[node] = append(nodePressure[node], item)
			}
			nodeSeen[node] = struct{}{}
		case ns != "" && pod != "":
			// pod-level 시계열. allow-list 에 막히면 양방향 모두에서 제외된다.
			if !namespaceAllowed(ns) {
				continue
			}
			ps := podSeries{
				namespace: ns,
				pod:       pod,
				podUID:    item.Series.Labels["src_pod_uid"],
				metric:    item.Metric,
				series:    item.Series,
			}
			if isLatencyVictim {
				podLatency[node] = append(podLatency[node], ps)
			} else {
				podPressure[node] = append(podPressure[node], ps)
			}
			nodeSeen[node] = struct{}{}
		}
	}

	// node 키를 정렬해 출력 순서를 결정적으로 만든다.
	nodeKeys := make([]string, 0, len(nodeSeen))
	for k := range nodeSeen {
		nodeKeys = append(nodeKeys, k)
	}
	sort.Strings(nodeKeys)

	sortNodeSeries := func(s []LabeledSeries) {
		sort.Slice(s, func(i, j int) bool { return s[i].Metric < s[j].Metric })
	}
	sortPodSeries := func(s []podSeries) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].namespace != s[j].namespace {
				return s[i].namespace < s[j].namespace
			}
			if s[i].pod != s[j].pod {
				return s[i].pod < s[j].pod
			}
			return s[i].metric < s[j].metric
		})
	}

	out := make([]CrossLevelPair, 0)
	for _, node := range nodeKeys {
		nPress := nodePressure[node]
		nLat := nodeLatency[node]
		pPress := podPressure[node]
		pLat := podLatency[node]
		sortNodeSeries(nPress)
		sortNodeSeries(nLat)
		sortPodSeries(pPress)
		sortPodSeries(pLat)

		// node 압박 → pod latency.
		for _, src := range nPress {
			for _, dst := range pLat {
				out = append(out, CrossLevelPair{
					Key: CrossLevelPairKey{
						Node:         node,
						Direction:    DirectionNodeToPod,
						PodNamespace: dst.namespace,
						Pod:          dst.pod,
						PodUID:       dst.podUID,
						SrcMetric:    src.Metric,
						DstMetric:    dst.metric,
					},
					Src: src.Series,
					Dst: dst.series,
				})
			}
		}
		// pod 압박 → node latency.
		for _, src := range pPress {
			for _, dst := range nLat {
				out = append(out, CrossLevelPair{
					Key: CrossLevelPairKey{
						Node:         node,
						Direction:    DirectionPodToNode,
						PodNamespace: src.namespace,
						Pod:          src.pod,
						PodUID:       src.podUID,
						SrcMetric:    src.metric,
						DstMetric:    dst.Metric,
					},
					Src: src.series,
					Dst: dst.Series,
				})
			}
		}
	}
	return out
}

// SelectTopNCrossLevel 은 CorrelationResult 슬라이스에서 IsCrossLevel=true 항목만 추출해 (node,
// direction, dimension) 그룹별 상위 topN pod 페어를 채택한다. 규칙은 SelectTopNCrossNode 와 동일
// 패턴이며 그룹 키와 라벨 셋만 cross-level 로 교체된다.
//
//   - IsCrossLevel == false 인 결과는 제외한다.
//   - Src 가 non-latency suspect 이고 Dst 가 latency victim 인 페어만 채택한다 (defense-in-depth).
//   - Status 가 StatusOK 또는 StatusPartial 인 결과만 채택한다.
//   - suspect 메트릭에서 dimension 을 분류해 DimensionUnknown 이면 제외한다.
//   - (node, direction, dimension, pod_namespace, pod, pod_uid) 단일 키로 max score dedup 한다. 한 pod
//     이 같은 dimension 에 매핑되는 압박 메트릭을 둘 이상 가질 수 있어 (예: cpu_throttle 와 host_compute_
//     stall 둘 다 cpu) max 1개만 채택한다.
//   - (node, direction, dimension) 그룹별 max_abs_value 내림차순 정렬 후 상위 topN 에 rank 1..topN 부여.
//     동률은 (pod_namespace, pod, pod_uid) lexicographic 순서로 결정한다.
//
// 결과는 (node, direction, dimension, rank) 순으로 정렬된 슬라이스다. topN <= 0 이면 nil 을 반환한다.
func SelectTopNCrossLevel(results []CorrelationResult, topN int) []CrossLevel {
	if topN <= 0 {
		return nil
	}

	type candidate struct {
		node          string
		direction     CrossLevelDirection
		podNamespace  string
		pod           string
		podUID        string
		suspectMetric string
		victimMetric  string
		dimension     ResourceDimension
		score         float64
		lag           int
		samples       int
		pvalue        float64
		grangerOK     bool
	}

	// #406 results 는 4개 레이어 전체 결과라 len(results) cap 사전 할당은 본 레이어 후보의 실제
	// 규모 (필터 통과분) 를 크게 웃돈다. append 의 상환 성장에 맡긴다.
	var candidates []candidate
	for _, r := range results {
		if !r.IsCrossLevel {
			continue
		}
		if r.Status != StatusOK && r.Status != StatusPartial {
			continue
		}
		// #150 cross-level victim 은 latency 단일로 유지한다. suspect 는 victim 이 아닌 압박이어야 한다.
		if isVictimMetric(r.CrossLevelPair.SrcMetric) {
			continue
		}
		if classifyVictimSignal(r.CrossLevelPair.DstMetric) != SignalLatency {
			continue
		}
		dim := classifyDimension(r.CrossLevelPair.SrcMetric)
		if dim == DimensionUnknown {
			continue
		}
		candidates = append(candidates, candidate{
			node:          r.CrossLevelPair.Node,
			direction:     r.CrossLevelPair.Direction,
			podNamespace:  r.CrossLevelPair.PodNamespace,
			pod:           r.CrossLevelPair.Pod,
			podUID:        r.CrossLevelPair.PodUID,
			suspectMetric: r.CrossLevelPair.SrcMetric,
			victimMetric:  r.CrossLevelPair.DstMetric,
			dimension:     dim,
			score:         r.MaxAbsValue,
			lag:           r.MaxAbsLag,
			samples:       r.SampleCount,
			pvalue:        r.PValue,
			grangerOK:     r.GrangerOK,
		})
	}

	type pairKey struct {
		node         string
		direction    CrossLevelDirection
		dimension    ResourceDimension
		podNamespace string
		pod          string
		podUID       string
	}
	byPair := make(map[pairKey]candidate)
	for _, c := range candidates {
		k := pairKey{c.node, c.direction, c.dimension, c.podNamespace, c.pod, c.podUID}
		if prev, ok := byPair[k]; !ok || c.score > prev.score {
			byPair[k] = c
		}
	}
	deduped := make([]candidate, 0, len(byPair))
	for _, c := range byPair {
		deduped = append(deduped, c)
	}

	type groupKey struct {
		node      string
		direction CrossLevelDirection
		dimension ResourceDimension
	}
	groups := make(map[groupKey][]candidate)
	for _, c := range deduped {
		k := groupKey{c.node, c.direction, c.dimension}
		groups[k] = append(groups[k], c)
	}

	groupKeys := make([]groupKey, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		a, b := groupKeys[i], groupKeys[j]
		if a.node != b.node {
			return a.node < b.node
		}
		if a.direction != b.direction {
			return a.direction < b.direction
		}
		return a.dimension < b.dimension
	})

	out := make([]CrossLevel, 0, len(deduped))
	for _, gk := range groupKeys {
		cands := groups[gk]
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].score != cands[j].score {
				return cands[i].score > cands[j].score
			}
			if cands[i].podNamespace != cands[j].podNamespace {
				return cands[i].podNamespace < cands[j].podNamespace
			}
			if cands[i].pod != cands[j].pod {
				return cands[i].pod < cands[j].pod
			}
			return cands[i].podUID < cands[j].podUID
		})
		limit := topN
		if len(cands) < limit {
			limit = len(cands)
		}
		for i := 0; i < limit; i++ {
			c := cands[i]
			out = append(out, CrossLevel{
				Node:          c.node,
				Direction:     c.direction,
				PodNamespace:  c.podNamespace,
				Pod:           c.pod,
				PodUID:        c.podUID,
				SuspectMetric: c.suspectMetric,
				VictimMetric:  c.victimMetric,
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
