package correlation

import "sort"

// #84 cross-node interference layer 의 라이브러리 entry point 다. node 단위 분석 layer 는 pod-level
// noisy neighbor (NoisyNeighbor / EnumeratePairs / SelectTopN) 와 독립이며 동일 cycle 에서 두 입도가
// 같은 데이터를 두 번 산출하지 않도록 본 파일 의 함수 셋 만 호출 한다.

// NodePairKey 는 cross-node 분석 의 페어 식별자 다. PairKey (pod 단위) 와 분리해 두 layer 의 라벨 셋
// 이 cross-eviction 되지 않게 한다. Src 가 suspect (자원 압박), Dst 가 victim (latency) 인 비대칭
// 의미 는 PairKey 와 동일 하다.
type NodePairKey struct {
	SrcNode   string `json:"src_node"`
	SrcMetric string `json:"src_metric"`
	DstNode   string `json:"dst_node"`
	DstMetric string `json:"dst_metric"`
}

// NodePair 는 NodePairKey 와 두 시계열 의 참조 다. PearsonWithLag 의 입력 으로 그대로 전달 가능 하다.
type NodePair struct {
	Key NodePairKey
	Src TimeSeries
	Dst TimeSeries
}

// NodeInterference 는 한 victim_node 의 한 dimension 에서 채택 된 단일 suspect_node 의 페어 정보 다.
// exporter 가 본 struct 를 correlation_cross_node_score{victim_node, suspect_node, dimension} 메트릭
// 으로 변환 한다.
type NodeInterference struct {
	VictimNode    string            `json:"victim_node"`
	VictimMetric  string            `json:"victim_metric"`
	SuspectNode   string            `json:"suspect_node"`
	SuspectMetric string            `json:"suspect_metric"`
	Dimension     ResourceDimension `json:"dimension"`
	Rank          int               `json:"rank"`
	Score         float64           `json:"score"`
	LagSteps      int               `json:"lag_steps"`
	SampleCount   int               `json:"sample_count"`
	PValue        float64           `json:"p_value"`
	GrangerOK     bool              `json:"granger_ok"`
}

// EnumerateNodePairs 는 입력 LabeledSeries 슬라이스 에서 node 단위 페어 를 생성 한다. 정책 은 다음과
// 같다.
//
//   - node 라벨 만 있고 src_namespace / src_pod 라벨 이 없는 node-level 시계열 만 후보 로 인정 한다.
//     pod 단위 시계열 (src_namespace / src_pod 채워 짐) 은 본 함수 의 schema 와 불일치 라 자동 제외 해
//     pod-level EnumeratePairs 와 의 입력 중복 을 차단 한다.
//   - victim_node != suspect_node 인 페어 만 생성 한다. 동일 노드 페어 는 same-node noisy neighbor
//     의 분석 자리 이며 본 layer 의 의도 와 어긋 난다.
//   - (X_node, Y_node) 와 (Y_node, X_node) 를 별도 페어 로 둘 다 생성 한다. 비대칭 분석 (X 자원 이 Y
//     latency 를 예측 vs Y 자원 이 X latency 를 예측) 을 위해서다.
//
// 노드 키 정렬 로 출력 순서 가 결정적 이며 단위 테스트 가 안정 적 으로 비교 가능 하다. 노드 단위 라
// cross-product N_nodes^2 만 발생 해 dev cluster 4 노드 기준 12 페어, prod 수십 노드 도 수백 페어 로
// cardinality 부담 이 거의 없다.
func EnumerateNodePairs(items []LabeledSeries) []NodePair {
	// 1단계: node 키 기준 그룹화. node-level 시계열 만 후보 로 모은다.
	byNode := make(map[string][]LabeledSeries)
	for _, item := range items {
		node := item.Series.Labels["node"]
		ns := item.Series.Labels["src_namespace"]
		pod := item.Series.Labels["src_pod"]
		if node == "" {
			continue
		}
		// pod 라벨 이 들어 있는 시계열 은 pod-level 분석 의 입력 이라 본 함수 가 자동 제외 한다.
		if ns != "" || pod != "" {
			continue
		}
		byNode[node] = append(byNode[node], item)
	}

	nodeKeys := make([]string, 0, len(byNode))
	for k := range byNode {
		nodeKeys = append(nodeKeys, k)
	}
	sort.Strings(nodeKeys)

	// 2단계: 노드 페어 enumerate. victim_node != suspect_node 인 모든 조합 을 양방향 으로 생성 한다.
	out := make([]NodePair, 0)
	for _, srcNode := range nodeKeys {
		srcGroup := byNode[srcNode]
		for _, dstNode := range nodeKeys {
			if srcNode == dstNode {
				continue
			}
			dstGroup := byNode[dstNode]
			for _, src := range srcGroup {
				for _, dst := range dstGroup {
					if src.Metric == dst.Metric {
						// 같은 metric 이지만 다른 노드 의 시계열 은 cross-node 비교 가 가능 하나
						// 본 PR 의 noisy neighbor 모델 (suspect 자원 압박 → victim latency) 에 부합
						// 하지 않 으므로 isLatencyMetric 분류 에 맡기지 않고 명시 제외 한다.
						continue
					}
					out = append(out, NodePair{
						Key: NodePairKey{
							SrcNode:   srcNode,
							SrcMetric: src.Metric,
							DstNode:   dstNode,
							DstMetric: dst.Metric,
						},
						Src: src.Series,
						Dst: dst.Series,
					})
				}
			}
		}
	}
	return out
}

// SelectTopNCrossNode 는 CorrelationResult 슬라이스 에서 IsCrossNode=true 항목 만 추출 해 cross-node
// noisy neighbor 의 (victim_node, dimension) 그룹별 상위 topN 페어 를 채택 한다. 다음 규칙 을 단정적
// 으로 적용 한다.
//
//   - IsCrossNode == false 인 결과 는 pod-level 분석 의 자리 이므로 본 함수 에서 제외 한다.
//   - 페어 정확히 한쪽 이 latency 메트릭 이고 반대쪽 이 non-latency cause score 인 페어 만 채택 한다.
//     pod-level SelectTopN 과 동일 조건 이다.
//   - Src 가 non-latency suspect, Dst 가 latency victim 인 방향 만 사용 한다. EnumerateNodePairs 가
//     만드는 반대 방향 (Y, X) 은 같은 (victim_node, suspect_node) 가 두 번 등장 하지 않도록 자동
//     dedup 의미 로 제외 한다.
//   - Status 가 StatusOK 또는 StatusPartial 인 결과 만 채택 한다.
//   - suspect 메트릭 에서 dimension 을 분류 해 DimensionUnknown 이면 제외 한다.
//   - (victim_node, suspect_node, dimension) 단일 키 로 max score dedup 한다.
//   - (victim_node, dimension) 그룹별 max_abs_value 내림차순 으로 정렬 해 상위 topN 개 에 rank 1..topN
//     부여 한다.
//
// 결과 는 (victim_node, dimension, rank) 순 으로 정렬 된 슬라이스 다. topN <= 0 이면 nil 을 반환 한다.
func SelectTopNCrossNode(results []CorrelationResult, topN int) []NodeInterference {
	if topN <= 0 {
		return nil
	}

	type candidate struct {
		victimNode    string
		victimMetric  string
		suspectNode   string
		suspectMetric string
		dimension     ResourceDimension
		score         float64
		lag           int
		samples       int
		pvalue        float64
		grangerOK     bool
	}

	candidates := make([]candidate, 0, len(results))
	for _, r := range results {
		if !r.IsCrossNode {
			continue
		}
		if r.Status != StatusOK && r.Status != StatusPartial {
			continue
		}
		srcLatency := isLatencyMetric(r.NodePair.SrcMetric)
		dstLatency := isLatencyMetric(r.NodePair.DstMetric)
		if srcLatency == dstLatency {
			continue
		}
		if srcLatency {
			continue
		}
		dim := classifyDimension(r.NodePair.SrcMetric)
		if dim == DimensionUnknown {
			continue
		}
		candidates = append(candidates, candidate{
			victimNode:    r.NodePair.DstNode,
			victimMetric:  r.NodePair.DstMetric,
			suspectNode:   r.NodePair.SrcNode,
			suspectMetric: r.NodePair.SrcMetric,
			dimension:     dim,
			score:         r.MaxAbsValue,
			lag:           r.MaxAbsLag,
			samples:       r.SampleCount,
			pvalue:        r.PValue,
			grangerOK:     r.GrangerOK,
		})
	}

	// (victim_node, suspect_node, dimension) 단일 키 dedup. 동일 키 의 multiple candidate (suspect
	// 메트릭 이 같은 dimension 에 매핑되는 두 시리즈 등) 중 max score 1 개 만 채택 한다.
	type pairKey struct {
		victimNode  string
		suspectNode string
		dimension   ResourceDimension
	}
	byPair := make(map[pairKey]candidate)
	for _, c := range candidates {
		k := pairKey{victimNode: c.victimNode, suspectNode: c.suspectNode, dimension: c.dimension}
		if prev, ok := byPair[k]; !ok || c.score > prev.score {
			byPair[k] = c
		}
	}
	deduped := make([]candidate, 0, len(byPair))
	for _, c := range byPair {
		deduped = append(deduped, c)
	}

	type groupKey struct {
		victimNode string
		dimension  ResourceDimension
	}
	groups := make(map[groupKey][]candidate)
	for _, c := range deduped {
		groups[groupKey{victimNode: c.victimNode, dimension: c.dimension}] = append(
			groups[groupKey{victimNode: c.victimNode, dimension: c.dimension}], c)
	}

	groupKeys := make([]groupKey, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		a, b := groupKeys[i], groupKeys[j]
		if a.victimNode != b.victimNode {
			return a.victimNode < b.victimNode
		}
		return a.dimension < b.dimension
	})

	out := make([]NodeInterference, 0)
	for _, gk := range groupKeys {
		cands := groups[gk]
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].score != cands[j].score {
				return cands[i].score > cands[j].score
			}
			return cands[i].suspectNode < cands[j].suspectNode
		})
		limit := topN
		if len(cands) < limit {
			limit = len(cands)
		}
		for i := 0; i < limit; i++ {
			c := cands[i]
			out = append(out, NodeInterference{
				VictimNode:    c.victimNode,
				VictimMetric:  c.victimMetric,
				SuspectNode:   c.suspectNode,
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
