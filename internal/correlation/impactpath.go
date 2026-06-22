package correlation

import "sort"

// #151 Phase 2: 다단계 영향 전파 경로 추출. Phase 1 의 ImpactGraph (정점 + suspect → victim 방향 엣지)
// 를 입력으로 받아 transitive 전파 경로를 추출하고 근원 suspect 를 식별한다. 한 suspect 의 영향이
// 중간 victim 을 거쳐 다른 대상으로 전파되는 다단계 경로를 명시적 경로 객체로 표현해 1-hop 페어
// 상관만으로는 보이지 않던 근본 원인 사슬을 드러낸다.

// ImpactPathHop 은 경로의 한 hop (한 엣지) 다. ImpactGraphEdge 에서 경로 표현에 필요한 필드만 추린다.
type ImpactPathHop struct {
	Suspect      PodIdentity       `json:"suspect"`
	Victim       PodIdentity       `json:"victim"`
	Dimension    ResourceDimension `json:"dimension"`
	VictimSignal VictimSignal      `json:"victim_signal"`
	Score        float64           `json:"score"`
	LagSteps     int               `json:"lag_steps"`
}

// RootKind 는 근원 (root) 의 식별 방식이다. 순환이 없는 그래프에서는 in-degree 0 인 순수 source 가
// 근원이지만, 조밀하거나 순환이 있는 그래프에는 순수 source 가 없을 수 있어 net-source (out>in) 로
// fallback 한다. 추후 SCC 응축 기반 근원 (RootKindSCC 등) 으로 확장 가능하도록 enum 으로 둔다.
type RootKind string

const (
	// RootKindSource 는 가지치기된 그래프에서 들어오는 강한 엣지가 없는 (in-degree 0) 순수 근원이다.
	RootKindSource RootKind = "source"
	// RootKindNetSource 는 순수 source 가 없을 때 채택하는 fallback 근원으로, out-degree 가 in-degree
	// 보다 큰 (보내는 영향이 받는 영향보다 많은) 정점이다.
	RootKindNetSource RootKind = "net_source"
)

// ImpactPath 는 근원 suspect (root) 에서 시작해 종착 victim (terminal) 으로 이어지는 다단계 전파
// 경로다. Hops 는 root 에서 terminal 까지의 순서 있는 엣지 열이며 같은 정점을 두 번 거치지 않는
// 단순 경로다. Score 는 weakest-link (hop 최소 score) 로, 경로 전체가 적어도 이 강도로 이어진다는
// 보수적 지표다. RootKind 는 root 가 순수 source 인지 net-source fallback 인지 알린다.
type ImpactPath struct {
	Root     PodIdentity     `json:"root"`
	RootKind RootKind        `json:"root_kind"`
	Terminal PodIdentity     `json:"terminal"`
	Hops     []ImpactPathHop `json:"hops"`
	Depth    int             `json:"depth"`
	Score    float64         `json:"score"`
}

// RootSuspect 는 근원 suspect (in-degree 0, out-degree>0 정점) 와 그 영향 범위다. Reach 는 본 root
// 에서 도달 가능한 distinct 종착 victim 수, PathCount 는 본 root 에서 시작하는 경로 수다. Reach 가
// 큰 root 는 가장 넓게 영향을 퍼뜨리는 근본 원인 후보다.
type RootSuspect struct {
	Root      PodIdentity `json:"root"`
	Kind      RootKind    `json:"kind"`
	Reach     int         `json:"reach"`
	PathCount int         `json:"path_count"`
}

// pathNodeKey 는 경로 순회의 정점 동일성이다. BuildImpactGraph 와 동일하게 (namespace, pod) 로 둬,
// uid 불일치로 같은 pod 가 여러 정점으로 갈라져 한 경로가 동일 pod 를 재방문 (의사 사이클) 하는 것을
// 막는다.
type pathNodeKey struct {
	namespace string
	pod       string
}

func podKey(id PodIdentity) pathNodeKey {
	return pathNodeKey{id.Namespace, id.Pod}
}

// ExtractImpactPaths 는 ImpactGraph 에서 근원 suspect 별 다단계 전파 경로를 추출한다. 정책은 다음과
// 같다.
//
//   - 근원 (root) 은 in-degree 0 이고 out-degree>0 인 정점이다. 들어오는 영향이 없는 전파 출발점이며,
//     순환만 있는 (root 가 없는) 컴포넌트는 추출 대상에서 자연 제외된다.
//   - 각 root 에서 out 엣지를 따라 DFS 로 단순 경로를 enumerate 한다. 같은 정점을 두 번 거치지 않아
//     사이클 (A→B→A) 에서 무한 순회하지 않는다.
//   - score 가 minScore 미만인 엣지는 약한 전파로 보고 가지치기한다.
//   - 더 따라갈 엣지가 없거나 maxDepth 에 도달하면 root→terminal maximal 경로 1 개를 emit 한다.
//   - 누적 경로 수가 maxPaths 캡에 도달하면 추출을 중단하고 truncated=true 를 반환한다 (조밀 그래프의
//     조합 폭발 방어, silent cap 방지).
//   - 순수 source (in-degree 0) 근원이 하나도 없으면 (조밀하거나 순환이 있는 그래프) net-source
//     (out-degree>in-degree) 정점으로 fallback 해 경로를 추출한다. 각 경로의 RootKind 로 어느 방식의
//     근원인지 알린다.
//
// 결과는 (root, terminal, score 내림차순) 정렬로 결정적이다. maxDepth<=0 면 5, maxPaths<=0 면 1024
// 로 fallback 한다. 반환값 truncated 는 maxPaths 캡으로 추출이 중단됐는지를 알린다.
func ExtractImpactPaths(g ImpactGraph, maxDepth int, minScore float64, maxPaths int) ([]ImpactPath, bool) {
	if maxDepth <= 0 {
		maxDepth = 5
	}
	if maxPaths <= 0 {
		maxPaths = 1024
	}

	// out 인접 리스트. minScore 이상 엣지만 후보로 둔다. 결정적 순회를 위해 victim 키 순 정렬한다.
	adj := make(map[pathNodeKey][]ImpactGraphEdge, len(g.Nodes))
	for _, e := range g.Edges {
		if e.Score < minScore {
			continue
		}
		k := podKey(e.Suspect)
		adj[k] = append(adj[k], e)
	}
	// victim pod 별로 정렬한다. 정점 동일성이 (namespace, pod) 라 같은 pod 쌍에 dimension / signal 만
	// 다른 평행 엣지가 있을 수 있어, victim pod 가 같으면 score 내림차순 (최강 엣지 우선) 으로 두고
	// 이어서 signal / dimension 으로 완전 결정적 순서를 만든다. 순회 시 victim pod 당 최강 엣지 1 개만
	// hop 으로 채택한다.
	for k := range adj {
		es := adj[k]
		sort.Slice(es, func(i, j int) bool {
			if es[i].Victim.Namespace != es[j].Victim.Namespace {
				return es[i].Victim.Namespace < es[j].Victim.Namespace
			}
			if es[i].Victim.Pod != es[j].Victim.Pod {
				return es[i].Victim.Pod < es[j].Victim.Pod
			}
			if es[i].Score != es[j].Score {
				return es[i].Score > es[j].Score
			}
			if es[i].VictimSignal != es[j].VictimSignal {
				return es[i].VictimSignal < es[j].VictimSignal
			}
			return es[i].Dimension < es[j].Dimension
		})
	}

	// root 는 가지치기된 (score>=minScore) 그래프 기준 in-degree 0, out-degree>0 정점이다. 순회가
	// 강한 엣지만 따라가므로 root 판정도 동일 그래프를 써야 정합한다. raw graph in-degree 를 쓰면 약한
	// 엣지 하나만 들어와도 root 에서 빠져, 강한 incoming 이 없는 진짜 근원이 누락된다. g.Nodes 가
	// (namespace, pod) 정렬 상태라 root 순회도 결정적이다.
	prunedIn := make(map[pathNodeKey]int)
	prunedOut := make(map[pathNodeKey]int)
	for sk, es := range adj {
		prunedOut[sk] = len(es)
		for _, e := range es {
			prunedIn[podKey(e.Victim)]++
		}
	}
	type rootEntry struct {
		node ImpactGraphNode
		kind RootKind
	}
	roots := make([]rootEntry, 0)
	for _, n := range g.Nodes {
		k := pathNodeKey{namespace: n.Namespace, pod: n.Pod}
		if prunedIn[k] == 0 && prunedOut[k] > 0 {
			roots = append(roots, rootEntry{node: n, kind: RootKindSource})
		}
	}
	// 순수 source 가 없으면 (조밀하거나 순환이 있는 그래프) net-source (out>in) 로 fallback 한다.
	// 들어오는 영향보다 내보내는 영향이 많은 정점이라 가장 상류에 가까운 근원 후보다.
	if len(roots) == 0 {
		for _, n := range g.Nodes {
			k := pathNodeKey{namespace: n.Namespace, pod: n.Pod}
			if prunedOut[k] > prunedIn[k] {
				roots = append(roots, rootEntry{node: n, kind: RootKindNetSource})
			}
		}
	}

	out := make([]ImpactPath, 0)
	truncated := false
	for _, r := range roots {
		rootID := PodIdentity{Namespace: r.node.Namespace, Pod: r.node.Pod, PodUID: r.node.PodUID}
		rootKind := r.kind
		visited := map[pathNodeKey]bool{podKey(rootID): true}
		hops := make([]ImpactPathHop, 0, maxDepth)
		var dfs func(cur pathNodeKey)
		dfs = func(cur pathNodeKey) {
			if len(out) >= maxPaths {
				truncated = true
				return
			}
			// 깊이 한계 도달 시 현재까지의 경로를 truncate 해 emit 한다. maxDepth>=1 이라 hops>0 이 보장된다.
			if len(hops) >= maxDepth {
				p := makePath(rootID, hops)
				p.RootKind = rootKind
				out = append(out, p)
				return
			}
			// adj[cur] 가 victim (namespace, pod) 정렬이라 같은 victim pod 로 가는 평행 엣지는 인접한다.
			// 직전 victim 과 비교해 건너뛰면 (정렬상 최강 score 가 먼저라 첫 엣지 채택) 재귀마다 map /
			// 슬라이스 할당 없이 victim pod 당 1 hop 만 채택한다.
			hasOutgoing := false
			var lastVictim pathNodeKey
			hasLast := false
			for _, e := range adj[cur] {
				vk := podKey(e.Victim)
				if hasLast && vk == lastVictim {
					continue
				}
				lastVictim = vk
				hasLast = true
				if visited[vk] {
					continue
				}
				hasOutgoing = true
				visited[vk] = true
				hops = append(hops, ImpactPathHop{
					Suspect:      e.Suspect,
					Victim:       e.Victim,
					Dimension:    e.Dimension,
					VictimSignal: e.VictimSignal,
					Score:        e.Score,
					LagSteps:     e.LagSteps,
				})
				dfs(vk)
				hops = hops[:len(hops)-1]
				visited[vk] = false
				if len(out) >= maxPaths {
					truncated = true
					return
				}
			}
			// 진행할 엣지가 없는 terminal 이면 root→terminal 경로 1 개 emit 한다 (hops>0 일 때만).
			if !hasOutgoing && len(hops) > 0 {
				p := makePath(rootID, hops)
				p.RootKind = rootKind
				out = append(out, p)
			}
		}
		dfs(podKey(rootID))
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Root.Namespace != b.Root.Namespace {
			return a.Root.Namespace < b.Root.Namespace
		}
		if a.Root.Pod != b.Root.Pod {
			return a.Root.Pod < b.Root.Pod
		}
		if a.Root.PodUID != b.Root.PodUID {
			return a.Root.PodUID < b.Root.PodUID
		}
		if a.Terminal.Namespace != b.Terminal.Namespace {
			return a.Terminal.Namespace < b.Terminal.Namespace
		}
		if a.Terminal.Pod != b.Terminal.Pod {
			return a.Terminal.Pod < b.Terminal.Pod
		}
		if a.Terminal.PodUID != b.Terminal.PodUID {
			return a.Terminal.PodUID < b.Terminal.PodUID
		}
		return a.Score > b.Score
	})
	return out, truncated
}

// makePath 는 hop 열에서 ImpactPath 를 만든다. Score 는 weakest-link (hop 최소), Terminal 은 마지막
// hop 의 victim 이다. hops 슬라이스는 caller 가 재사용 (truncate) 하므로 복사본을 보관한다.
func makePath(root PodIdentity, hops []ImpactPathHop) ImpactPath {
	// 방어적 가드: caller 는 hops>0 일 때만 호출하지만 빈 hop 으로 들어와도 panic 대신 root 만 담은
	// 경로를 돌려준다.
	if len(hops) == 0 {
		return ImpactPath{Root: root, Terminal: root}
	}
	copied := append([]ImpactPathHop(nil), hops...)
	score := copied[0].Score
	for _, h := range copied[1:] {
		if h.Score < score {
			score = h.Score
		}
	}
	return ImpactPath{
		Root:     root,
		Terminal: copied[len(copied)-1].Victim,
		Hops:     copied,
		Depth:    len(copied),
		Score:    score,
	}
}

// RootSuspects 는 추출된 경로를 root 별로 집계해 영향 범위 (distinct 종착 victim 수) 와 경로 수를
// 산정한다. Reach 내림차순, 동률 시 (namespace, pod) 순으로 정렬되어 가장 넓게 퍼뜨리는 근본 원인
// 후보가 앞에 온다.
func RootSuspects(paths []ImpactPath) []RootSuspect {
	type agg struct {
		root      PodIdentity
		kind      RootKind
		terminals map[pathNodeKey]struct{}
		pathCount int
	}
	byRoot := make(map[pathNodeKey]*agg)
	for _, p := range paths {
		k := podKey(p.Root)
		a, ok := byRoot[k]
		if !ok {
			a = &agg{root: p.Root, kind: p.RootKind, terminals: make(map[pathNodeKey]struct{})}
			byRoot[k] = a
		}
		a.terminals[podKey(p.Terminal)] = struct{}{}
		a.pathCount++
	}

	out := make([]RootSuspect, 0, len(byRoot))
	for _, a := range byRoot {
		out = append(out, RootSuspect{Root: a.root, Kind: a.kind, Reach: len(a.terminals), PathCount: a.pathCount})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reach != out[j].Reach {
			return out[i].Reach > out[j].Reach
		}
		if out[i].Root.Namespace != out[j].Root.Namespace {
			return out[i].Root.Namespace < out[j].Root.Namespace
		}
		if out[i].Root.Pod != out[j].Root.Pod {
			return out[i].Root.Pod < out[j].Root.Pod
		}
		return out[i].Root.PodUID < out[j].Root.PodUID
	})
	return out
}
