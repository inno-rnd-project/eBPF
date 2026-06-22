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

// EnumerateNodePairs 는 입력 LabeledSeries 슬라이스에서 node 단위 페어를 생성한다. 정책은 다음과
// 같다.
//
//   - node 라벨만 있고 src_namespace / src_pod 라벨이 없는 node-level 시계열만 후보로 인정한다.
//     pod 단위 시계열 (src_namespace / src_pod 채워짐) 은 본 함수의 schema와 불일치라 자동 제외해
//     pod-level EnumeratePairs와의 입력 중복을 차단한다.
//   - victim_node != suspect_node 인 페어만 생성한다. 동일 노드 페어는 same-node noisy neighbor의
//     분석 자리이며 본 layer의 의도와 어긋난다.
//   - Src가 non-latency suspect (pressure score), Dst가 latency victim인 단일 방향 페어만 생성한다.
//     noisy neighbor 모델 (suspect 자원 압박이 victim latency를 예측) 에 부합하지 않는 방향
//     (latency → pressure, pressure → pressure, latency → latency) 은 SelectTopNCrossNode 단계에서도
//     모두 필터링되므로 enumerate 단계에서 미리 제외해 Granger 인과성 검정의 행렬 연산 비용을
//     회피한다. dev cluster 4 노드 기준 192 페어 중 48 페어만 유효해 약 75% 비용 절감이 가능하다.
//
// 노드 키 정렬로 출력 순서가 결정적이며 단위 테스트가 안정적으로 비교 가능하다. 노드 단위라
// cross-product N_nodes^2만 발생해 dev cluster 4 노드 기준 12 페어 디렉션, prod 수십 노드도 수백
// 페어로 cardinality 부담이 거의 없다.
func EnumerateNodePairs(items []LabeledSeries) []NodePair {
	// 1단계: node 키 기준 그룹화. node-level 시계열만 후보로 모은다.
	byNode := make(map[string][]LabeledSeries)
	for _, item := range items {
		node := item.Series.Labels["node"]
		ns := item.Series.Labels["src_namespace"]
		pod := item.Series.Labels["src_pod"]
		if node == "" {
			continue
		}
		// pod 라벨이 들어 있는 시계열은 pod-level 분석의 입력이라 본 함수가 자동 제외한다.
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

	// 2단계: Src가 non-latency suspect이고 Dst가 latency victim인 단일 방향 페어만 enumerate.
	out := make([]NodePair, 0)
	for _, srcNode := range nodeKeys {
		srcGroup := byNode[srcNode]
		for _, dstNode := range nodeKeys {
			if srcNode == dstNode {
				continue
			}
			dstGroup := byNode[dstNode]
			for _, src := range srcGroup {
				// #150 suspect 는 victim (latency / throughput / error) 이 아닌 cause 만 인정한다.
				if isVictimMetric(src.Metric) {
					continue
				}
				for _, dst := range dstGroup {
					// cross-node victim 은 latency 단일로 유지한다 (node throughput/error 미도입).
					if classifyVictimSignal(dst.Metric) != SignalLatency {
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

// SelectTopNCrossNode 는 CorrelationResult 슬라이스에서 IsCrossNode=true 항목만 추출해 cross-node
// noisy neighbor의 (victim_node, dimension) 그룹별 상위 topN 페어를 채택한다. 다음 규칙을 단정적
// 으로 적용한다.
//
//   - IsCrossNode == false 인 결과는 pod-level 분석의 자리이므로 본 함수에서 제외한다.
//   - 페어 정확히 한쪽이 latency 메트릭이고 반대쪽이 non-latency cause score인 페어만 채택한다.
//     EnumerateNodePairs가 사전 필터로 Src=non-latency / Dst=latency 방향만 enumerate하므로 본 두
//     조건은 정상 동작 경로에서는 항상 통과한다. 다른 호출 경로 (직접 CorrelationResult 슬라이스를
//     주입하는 단위 테스트 등) 의 안전성을 위해 defense-in-depth로 검증 단계를 유지한다.
//   - Status가 StatusOK 또는 StatusPartial인 결과만 채택한다.
//   - suspect 메트릭에서 dimension을 분류해 DimensionUnknown이면 제외한다.
//   - (victim_node, suspect_node, dimension) 단일 키로 max score dedup한다.
//   - (victim_node, dimension) 그룹별 max_abs_value 내림차순으로 정렬해 상위 topN 개에 rank 1..topN
//     부여한다.
//
// 결과는 (victim_node, dimension, rank) 순으로 정렬된 슬라이스다. topN <= 0 이면 nil을 반환한다.
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
		// defense-in-depth. EnumerateNodePairs가 사전 필터로 Src=suspect / Dst=latency victim 만
		// enumerate 하므로 본 두 조건은 정상 경로에서 항상 통과하나 단위 테스트 등의 직접 주입 경로
		// 안전성을 위해 검증을 유지한다. #150 victim 은 cross-node 에서 latency 단일로 유지한다.
		if isVictimMetric(r.NodePair.SrcMetric) {
			continue
		}
		if classifyVictimSignal(r.NodePair.DstMetric) != SignalLatency {
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
