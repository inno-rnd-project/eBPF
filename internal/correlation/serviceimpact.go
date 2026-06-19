package correlation

import "sort"

// #148 service-impact layer 의 라이브러리 entry point 다. victim 을 K8s Service 에 근사 하는 workload
// (Deployment / owner) 단위 latency 로 집계 해, suspect node 의 자원 압박 이 어느 service 의 latency
// 에 영향 을 주는지 산정 한다. pod-level (noisy neighbor) 과 node-level (cross-node) layer 와 독립이며
// 동일 cycle 에서 세 입도가 같은 데이터를 중복 산출 하지 않도록 본 파일의 함수 셋만 호출한다.
//
// victim 의 K8s Service 는 netobs 가 emit 하는 src_workload 라벨 (resolver 의 owner 해석 결과) 로
// 근사한다. Service selector 기반 멤버십의 정식 해석은 correlation-exporter 의 K8s API 무접근 설계를
// 깨므로 본 layer scope 밖이며, 대부분의 운영 환경에서 1 Deployment 가 1 Service 를 뒷받침해 workload
// 라벨 집계가 service 단위 영향과 사실상 일치한다. suspect 를 pod 단위로 확장하는 것은 follow-up 이다.

// ServiceImpactPairKey 는 service-impact 분석의 페어 식별자다. Src 가 suspect (node 자원 압박),
// Dst 가 victim (workload latency) 인 비대칭 의미는 PairKey / NodePairKey 와 동일하다.
type ServiceImpactPairKey struct {
	SuspectNode     string `json:"suspect_node"`
	SuspectMetric   string `json:"suspect_metric"`
	VictimNamespace string `json:"victim_namespace"`
	VictimWorkload  string `json:"victim_workload"`
	VictimMetric    string `json:"victim_metric"`
}

// ServiceImpactPair 는 ServiceImpactPairKey 와 두 시계열의 참조다. PearsonWithLag 입력으로 그대로
// 전달 가능하다.
type ServiceImpactPair struct {
	Key ServiceImpactPairKey
	Src TimeSeries
	Dst TimeSeries
}

// ServiceImpact 는 한 victim workload 의 한 dimension 에서 채택된 단일 suspect node 의 페어 정보다.
// exporter 가 본 struct 를 correlation_service_impact_score{victim_namespace, victim_workload,
// suspect_node, dimension} 메트릭으로 변환한다.
type ServiceImpact struct {
	VictimNamespace string            `json:"victim_namespace"`
	VictimWorkload  string            `json:"victim_workload"`
	VictimMetric    string            `json:"victim_metric"`
	SuspectNode     string            `json:"suspect_node"`
	SuspectMetric   string            `json:"suspect_metric"`
	Dimension       ResourceDimension `json:"dimension"`
	Rank            int               `json:"rank"`
	Score           float64           `json:"score"`
	LagSteps        int               `json:"lag_steps"`
	SampleCount     int               `json:"sample_count"`
	PValue          float64           `json:"p_value"`
	GrangerOK       bool              `json:"granger_ok"`
}

// EnumerateServiceImpactPairs 는 입력 LabeledSeries 슬라이스에서 (suspect node, victim workload) 페어를
// 생성한다. 정책은 다음과 같다.
//
//   - suspect 후보는 node 라벨만 있고 src_namespace / src_pod / src_workload 가 비어 있는 node-level
//     non-latency 시계열이다 (예: node:cpu_pressure_score:5m). node-level latency 와 pod-level /
//     workload-level 시계열은 schema 불일치로 자동 제외된다.
//   - victim 후보는 src_namespace 와 src_workload 가 모두 채워지고 src_pod 가 비어 있는 workload-level
//     latency 시계열이다 (예: workload:netobs_stage_latency_p99:5m). pod-level latency (src_pod 보유) 와
//     node-level latency (src_namespace 없음) 는 제외되어 세 layer 의 입력이 라벨 schema 로 분리된다.
//   - Src=suspect, Dst=victim 단일 방향 페어만 생성한다. noisy neighbor 모델 (suspect 압박이 victim
//     latency 를 예측) 에 부합하지 않는 방향은 enumerate 단계에서 미리 제외해 Granger 행렬 연산 비용을
//     회피한다.
//
// suspect 는 (node, metric), victim 은 (namespace, workload, metric) 정렬로 출력 순서가 결정적이라
// 단위 테스트가 안정적으로 비교 가능하다. cross-product 는 N_nodes * pressure_dim * N_workloads 라
// 노드 수가 적어 cardinality 부담이 작다.
func EnumerateServiceImpactPairs(items []LabeledSeries) []ServiceImpactPair {
	type victimSeries struct {
		namespace string
		workload  string
		metric    string
		series    TimeSeries
	}
	suspects := make([]LabeledSeries, 0)
	victims := make([]victimSeries, 0)
	for _, item := range items {
		node := item.Series.Labels["node"]
		ns := item.Series.Labels["src_namespace"]
		pod := item.Series.Labels["src_pod"]
		wl := item.Series.Labels["src_workload"]
		switch {
		case node != "" && ns == "" && pod == "" && wl == "" && !isLatencyMetric(item.Metric):
			suspects = append(suspects, item)
		case ns != "" && wl != "" && pod == "" && isLatencyMetric(item.Metric):
			victims = append(victims, victimSeries{namespace: ns, workload: wl, metric: item.Metric, series: item.Series})
		}
	}

	sort.Slice(suspects, func(i, j int) bool {
		ni, nj := suspects[i].Series.Labels["node"], suspects[j].Series.Labels["node"]
		if ni != nj {
			return ni < nj
		}
		return suspects[i].Metric < suspects[j].Metric
	})
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].namespace != victims[j].namespace {
			return victims[i].namespace < victims[j].namespace
		}
		if victims[i].workload != victims[j].workload {
			return victims[i].workload < victims[j].workload
		}
		return victims[i].metric < victims[j].metric
	})

	out := make([]ServiceImpactPair, 0, len(suspects)*len(victims))
	for _, s := range suspects {
		node := s.Series.Labels["node"]
		for _, v := range victims {
			out = append(out, ServiceImpactPair{
				Key: ServiceImpactPairKey{
					SuspectNode:     node,
					SuspectMetric:   s.Metric,
					VictimNamespace: v.namespace,
					VictimWorkload:  v.workload,
					VictimMetric:    v.metric,
				},
				Src: s.Series,
				Dst: v.series,
			})
		}
	}
	return out
}

// SelectTopNServiceImpact 는 CorrelationResult 슬라이스에서 IsServiceImpact=true 항목만 추출해
// (victim_namespace, victim_workload, dimension) 그룹별 상위 topN suspect node 페어를 채택한다. 규칙은
// SelectTopNCrossNode 와 동일 패턴이며 라벨 셋만 workload-level victim 으로 교체된다.
//
//   - IsServiceImpact == false 인 결과는 제외한다.
//   - Src 가 non-latency suspect 이고 Dst 가 latency victim 인 페어만 채택한다. EnumerateServiceImpactPairs
//     가 사전 필터로 본 방향만 enumerate 하므로 정상 경로에서는 항상 통과하나 직접 주입 경로 (단위
//     테스트 등) 안전성을 위해 defense-in-depth 로 검증을 유지한다.
//   - Status 가 StatusOK 또는 StatusPartial 인 결과만 채택한다.
//   - suspect 메트릭에서 dimension 을 분류해 DimensionUnknown 이면 제외한다.
//   - (victim_namespace, victim_workload, suspect_node, dimension) 단일 키로 max score dedup 한다.
//   - (victim_namespace, victim_workload, dimension) 그룹별 max_abs_value 내림차순 정렬 후 상위 topN 에
//     rank 1..topN 부여한다. 동률은 suspect_node lexicographic 순서로 결정한다.
//
// 결과는 (victim_namespace, victim_workload, dimension, rank) 순으로 정렬된 슬라이스다. topN <= 0 이면
// nil 을 반환한다.
func SelectTopNServiceImpact(results []CorrelationResult, topN int) []ServiceImpact {
	if topN <= 0 {
		return nil
	}

	type candidate struct {
		victimNamespace string
		victimWorkload  string
		victimMetric    string
		suspectNode     string
		suspectMetric   string
		dimension       ResourceDimension
		score           float64
		lag             int
		samples         int
		pvalue          float64
		grangerOK       bool
	}

	candidates := make([]candidate, 0, len(results))
	for _, r := range results {
		if !r.IsServiceImpact {
			continue
		}
		if r.Status != StatusOK && r.Status != StatusPartial {
			continue
		}
		srcLatency := isLatencyMetric(r.ServiceImpactPair.SuspectMetric)
		dstLatency := isLatencyMetric(r.ServiceImpactPair.VictimMetric)
		if srcLatency == dstLatency {
			continue
		}
		if srcLatency {
			continue
		}
		dim := classifyDimension(r.ServiceImpactPair.SuspectMetric)
		if dim == DimensionUnknown {
			continue
		}
		candidates = append(candidates, candidate{
			victimNamespace: r.ServiceImpactPair.VictimNamespace,
			victimWorkload:  r.ServiceImpactPair.VictimWorkload,
			victimMetric:    r.ServiceImpactPair.VictimMetric,
			suspectNode:     r.ServiceImpactPair.SuspectNode,
			suspectMetric:   r.ServiceImpactPair.SuspectMetric,
			dimension:       dim,
			score:           r.MaxAbsValue,
			lag:             r.MaxAbsLag,
			samples:         r.SampleCount,
			pvalue:          r.PValue,
			grangerOK:       r.GrangerOK,
		})
	}

	// (victim_namespace, victim_workload, suspect_node, dimension) 단일 키 dedup. 동일 키의 multiple
	// candidate (suspect 메트릭이 같은 dimension 에 매핑되는 두 시리즈 등) 중 max score 1개만 채택한다.
	type pairKey struct {
		victimNamespace string
		victimWorkload  string
		suspectNode     string
		dimension       ResourceDimension
	}
	byPair := make(map[pairKey]candidate)
	for _, c := range candidates {
		k := pairKey{c.victimNamespace, c.victimWorkload, c.suspectNode, c.dimension}
		if prev, ok := byPair[k]; !ok || c.score > prev.score {
			byPair[k] = c
		}
	}
	deduped := make([]candidate, 0, len(byPair))
	for _, c := range byPair {
		deduped = append(deduped, c)
	}

	type groupKey struct {
		victimNamespace string
		victimWorkload  string
		dimension       ResourceDimension
	}
	groups := make(map[groupKey][]candidate)
	for _, c := range deduped {
		k := groupKey{c.victimNamespace, c.victimWorkload, c.dimension}
		groups[k] = append(groups[k], c)
	}

	groupKeys := make([]groupKey, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		a, b := groupKeys[i], groupKeys[j]
		if a.victimNamespace != b.victimNamespace {
			return a.victimNamespace < b.victimNamespace
		}
		if a.victimWorkload != b.victimWorkload {
			return a.victimWorkload < b.victimWorkload
		}
		return a.dimension < b.dimension
	})

	// out 의 capacity 는 deduped 길이로 잡는다. 각 그룹이 min(topN, 그룹 크기) 개만 채택하므로 출력
	// 총합은 그룹 크기 합 (= len(deduped)) 이하이며, suspect node 수가 그룹당 topN 보다 작은 흔한
	// 케이스에서 len(groupKeys)*topN 보다 과다 할당을 줄이는 정확한 상한이다.
	out := make([]ServiceImpact, 0, len(deduped))
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
			out = append(out, ServiceImpact{
				VictimNamespace: c.victimNamespace,
				VictimWorkload:  c.victimWorkload,
				VictimMetric:    c.victimMetric,
				SuspectNode:     c.suspectNode,
				SuspectMetric:   c.suspectMetric,
				Dimension:       c.dimension,
				Rank:            i + 1,
				Score:           c.score,
				LagSteps:        c.lag,
				SampleCount:     c.samples,
				PValue:          c.pvalue,
				GrangerOK:       c.grangerOK,
			})
		}
	}
	return out
}
