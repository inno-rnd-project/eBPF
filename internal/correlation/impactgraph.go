package correlation

import "sort"

// #151 Phase 1: 영향 전파 그래프 구성. 기존 분석은 1-hop 페어 상관 (suspect → victim) 을 flat
// 슬라이스로만 표현해 한 suspect 의 영향이 중간 victim 을 거쳐 다른 대상으로 전파되는 다단계 경로를
// 추적하지 못했다. 본 파일은 noisy neighbor Top-N 을 정점 (pod) 과 방향 가중 엣지 (suspect → victim)
// 로 하는 in-memory 그래프로 재구성해 다단계 경로 추출 (Phase 2) 의 토대를 만든다.
//
// 이슈 비목표대로 본 PR 은 그래프 구성과 노출까지만 다루며, transitive 경로 추출과 근원 suspect
// 식별은 후속 PR (Phase 2) 에서 본 그래프를 입력으로 진행한다.

// ImpactGraphEdge 는 suspect → victim 1-hop 방향 엣지다. NoisyNeighbor 한 항목이 한 엣지가 되며,
// 한 victim 이 자신의 압박 (suspect 역할) 으로 또 다른 victim 에 영향을 주면 그 pod 는 한 엣지의
// victim 이자 다른 엣지의 suspect 가 되어 다단계 경로의 중간 노드가 된다.
type ImpactGraphEdge struct {
	Suspect      PodIdentity       `json:"suspect"`
	Victim       PodIdentity       `json:"victim"`
	Dimension    ResourceDimension `json:"dimension"`
	VictimSignal VictimSignal      `json:"victim_signal"`
	Score        float64           `json:"score"`
	LagSteps     int               `json:"lag_steps"`
	PValue       float64           `json:"p_value"`
	GrangerOK    bool              `json:"granger_ok"`
}

// ImpactGraphNode 는 그래프의 정점 (pod) 이다. OutDegree 는 이 pod 가 suspect 인 엣지 수 (영향을 주는
// 관계), InDegree 는 victim 인 엣지 수 (영향을 받는 관계) 다. OutDegree 가 크고 InDegree 가 0 에
// 가까운 pod 는 다단계 전파의 근원 suspect 후보, InDegree 가 큰 pod 는 영향이 모이는 victim hub 다.
type ImpactGraphNode struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	PodUID    string `json:"pod_uid"`
	OutDegree int    `json:"out_degree"`
	InDegree  int    `json:"in_degree"`
}

// ImpactGraph 는 정점과 방향 엣지로 구성된 영향 전파 그래프다. Nodes 와 Edges 는 결정적 순서로
// 정렬되어 동일 cycle 의 재현성과 단위 테스트 안정성을 보장한다.
type ImpactGraph struct {
	Nodes []ImpactGraphNode `json:"nodes"`
	Edges []ImpactGraphEdge `json:"edges"`
}

// BuildImpactGraph 는 noisy neighbor Top-N 슬라이스에서 영향 전파 그래프를 구성한다. 각 NoisyNeighbor
// 가 suspect → victim 방향 엣지가 되고, suspect 와 victim pod 가 정점이 된다. 정점 키는 (namespace,
// pod, pod_uid) 이며 한 pod 가 여러 엣지에 등장하면 out / in degree 가 누적된다.
//
// 입력이 이미 SelectTopN 으로 필터 / 데듀프 / topN 컷된 결과라 그래프 크기가 통제되며, 본 Phase 1 은
// pod-level noisy neighbor 만 정점으로 둔다 (node / workload 입도 그래프는 Phase 2 확장 대상).
func BuildImpactGraph(neighbors []NoisyNeighbor) ImpactGraph {
	edges := make([]ImpactGraphEdge, 0, len(neighbors))
	for _, nb := range neighbors {
		edges = append(edges, ImpactGraphEdge{
			Suspect:      nb.Suspect,
			Victim:       nb.Victim,
			Dimension:    nb.Dimension,
			VictimSignal: nb.VictimSignal,
			Score:        nb.Score,
			LagSteps:     nb.LagSteps,
			PValue:       nb.PValue,
			GrangerOK:    nb.GrangerOK,
		})
	}
	sortImpactEdges(edges)
	return ImpactGraph{Nodes: nodesFromEdges(edges), Edges: edges}
}

// Filter 는 엣지를 namespace 와 min_score 로 거른 유도 부분그래프를 반환한다. namespace 가 비어 있지
// 않으면 suspect 또는 victim 이 그 namespace 인 엣지만, min_score>0 이면 score 가 그 이상인 엣지만
// 남긴다. 정점과 degree 는 걸러진 엣지에서 재산정되어 부분그래프 내부 정합이 유지된다. /api/v1/
// impact-graph 가 대형 그래프 응답을 좁히는 데 쓴다.
func (g ImpactGraph) Filter(namespace string, minScore float64) ImpactGraph {
	filtered := make([]ImpactGraphEdge, 0, len(g.Edges))
	for _, e := range g.Edges {
		if minScore > 0 && e.Score < minScore {
			continue
		}
		if namespace != "" && e.Suspect.Namespace != namespace && e.Victim.Namespace != namespace {
			continue
		}
		filtered = append(filtered, e)
	}
	// g.Edges 가 이미 정렬돼 있어 filtered 도 정렬 순서를 보존한다. 정점만 재산정한다.
	return ImpactGraph{Nodes: nodesFromEdges(filtered), Edges: filtered}
}

// nodesFromEdges 는 엣지 집합에서 정점 (out/in degree 포함) 을 재구성한다. BuildImpactGraph 와 Filter
// 가 공유한다. 정점 동일성은 (namespace, pod) 다. src_pod_uid 가 suspect 측 score (sum by(node,
// namespace,pod)) 와 victim 측 latency 에서 일관되게 채워지지 않아, 같은 pod 가 uid="" 노드와 uid=set
// 노드로 갈라져 정점이 중복되고 경로에 의사 사이클이 생기는 것을 막는다. PodUID 는 비어 있지 않은
// 최소 uid 를 채택해 입력 순서와 무관하게 결정적이다.
func nodesFromEdges(edges []ImpactGraphEdge) []ImpactGraphNode {
	type nodeKey struct {
		namespace string
		pod       string
	}
	nodes := make(map[nodeKey]*ImpactGraphNode, len(edges))
	getNode := func(id PodIdentity) *ImpactGraphNode {
		k := nodeKey{id.Namespace, id.Pod}
		n, ok := nodes[k]
		if !ok {
			n = &ImpactGraphNode{Namespace: id.Namespace, Pod: id.Pod, PodUID: id.PodUID}
			nodes[k] = n
		}
		if id.PodUID != "" && (n.PodUID == "" || id.PodUID < n.PodUID) {
			n.PodUID = id.PodUID
		}
		return n
	}
	for _, e := range edges {
		getNode(e.Suspect).OutDegree++
		getNode(e.Victim).InDegree++
	}
	out := make([]ImpactGraphNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].PodUID < out[j].PodUID
	})
	return out
}

// sortImpactEdges 는 엣지를 결정적 순서로 정렬한다. 키는 (suspect ns/pod/uid, victim ns/pod/uid,
// victim_signal, dimension) 다. SelectTopN 이 동일 키로 dedup 하므로 본 키가 엣지마다 유일해 결정적
// 이며, 동명 pod 가 재생성되어 UID 만 다르게 공존해도 순서가 흔들리지 않는다.
func sortImpactEdges(edges []ImpactGraphEdge) {
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Suspect.Namespace != b.Suspect.Namespace {
			return a.Suspect.Namespace < b.Suspect.Namespace
		}
		if a.Suspect.Pod != b.Suspect.Pod {
			return a.Suspect.Pod < b.Suspect.Pod
		}
		if a.Suspect.PodUID != b.Suspect.PodUID {
			return a.Suspect.PodUID < b.Suspect.PodUID
		}
		if a.Victim.Namespace != b.Victim.Namespace {
			return a.Victim.Namespace < b.Victim.Namespace
		}
		if a.Victim.Pod != b.Victim.Pod {
			return a.Victim.Pod < b.Victim.Pod
		}
		if a.Victim.PodUID != b.Victim.PodUID {
			return a.Victim.PodUID < b.Victim.PodUID
		}
		if a.VictimSignal != b.VictimSignal {
			return a.VictimSignal < b.VictimSignal
		}
		return a.Dimension < b.Dimension
	})
}
