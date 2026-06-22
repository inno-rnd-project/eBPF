package correlation

import "testing"

// graphFromEdges 는 (victimPod, suspectPod, score) 트리플에서 ImpactGraph 를 만든다. ngEdge 는
// impactgraph_test.go 에 정의돼 있어 재사용한다.
func graphFromEdges(edges ...[3]interface{}) ImpactGraph {
	ns := make([]NoisyNeighbor, 0, len(edges))
	for _, e := range edges {
		ns = append(ns, ngEdge(e[0].(string), e[1].(string), DimensionCPU, SignalLatency, e[2].(float64)))
	}
	return BuildImpactGraph(ns)
}

func pathTerminals(paths []ImpactPath) map[string]ImpactPath {
	out := map[string]ImpactPath{}
	for _, p := range paths {
		out[p.Root.Pod+"->"+p.Terminal.Pod] = p
	}
	return out
}

// TestExtractImpactPaths_Chain 은 a→b→c 사슬에서 root=a, terminal=c, depth=2, score=weakest-link 인
// 다단계 경로가 추출되는지 검증한다.
func TestExtractImpactPaths_Chain(t *testing.T) {
	g := graphFromEdges([3]interface{}{"b", "a", 0.9}, [3]interface{}{"c", "b", 0.7})
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	if len(paths) != 1 {
		t.Fatalf("paths=%d want 1 (a→b→c)", len(paths))
	}
	p := paths[0]
	if p.Root.Pod != "a" || p.Terminal.Pod != "c" {
		t.Errorf("root=%s terminal=%s want a/c", p.Root.Pod, p.Terminal.Pod)
	}
	if p.Depth != 2 {
		t.Errorf("depth=%d want 2", p.Depth)
	}
	if p.Score != 0.7 {
		t.Errorf("score=%v want 0.7 (weakest-link min(0.9,0.7))", p.Score)
	}
	if len(p.Hops) != 2 || p.Hops[0].Suspect.Pod != "a" || p.Hops[1].Victim.Pod != "c" {
		t.Errorf("hops 구성 오류: %+v", p.Hops)
	}
}

// TestExtractImpactPaths_Branch 는 root 가 여러 terminal 로 분기하면 경로가 각각 추출되는지 검증한다.
func TestExtractImpactPaths_Branch(t *testing.T) {
	g := graphFromEdges([3]interface{}{"b", "a", 0.9}, [3]interface{}{"c", "a", 0.6})
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	if len(paths) != 2 {
		t.Fatalf("paths=%d want 2 (a→b, a→c)", len(paths))
	}
	pt := pathTerminals(paths)
	if _, ok := pt["a->b"]; !ok {
		t.Errorf("a->b 경로 누락")
	}
	if _, ok := pt["a->c"]; !ok {
		t.Errorf("a->c 경로 누락")
	}
}

// TestExtractImpactPaths_PureCycleNoRoot 는 in-degree 0 정점이 없는 순환 그래프에서 root 가 없어
// 경로가 0 인지 검증한다 (사이클에서 무한 순회하지 않음).
func TestExtractImpactPaths_PureCycleNoRoot(t *testing.T) {
	g := graphFromEdges([3]interface{}{"b", "a", 0.9}, [3]interface{}{"a", "b", 0.8})
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	if len(paths) != 0 {
		t.Errorf("paths=%d want 0 (root 없는 순환)", len(paths))
	}
}

// TestExtractImpactPaths_CycleWithRoot 는 root 진입 후 사이클이 있어도 단순 경로로 안전히 추출되는지
// 검증한다. r→a, a→b, b→a 에서 b→a 는 a 가 이미 방문돼 차단된다.
func TestExtractImpactPaths_CycleWithRoot(t *testing.T) {
	g := graphFromEdges(
		[3]interface{}{"a", "r", 0.9},
		[3]interface{}{"b", "a", 0.8},
		[3]interface{}{"a", "b", 0.7},
	)
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	if len(paths) != 1 {
		t.Fatalf("paths=%d want 1 (r→a→b)", len(paths))
	}
	if paths[0].Root.Pod != "r" || paths[0].Terminal.Pod != "b" || paths[0].Depth != 2 {
		t.Errorf("path=%s→%s depth=%d want r→b/2", paths[0].Root.Pod, paths[0].Terminal.Pod, paths[0].Depth)
	}
}

// TestExtractImpactPaths_MinScorePrune 는 minScore 미만 엣지가 가지치기되어 경로가 거기서 끊기는지
// 검증한다.
func TestExtractImpactPaths_MinScorePrune(t *testing.T) {
	g := graphFromEdges([3]interface{}{"b", "a", 0.9}, [3]interface{}{"c", "b", 0.3})
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	if len(paths) != 1 {
		t.Fatalf("paths=%d want 1 (b→c 가지치기로 a→b 만)", len(paths))
	}
	if paths[0].Terminal.Pod != "b" || paths[0].Depth != 1 {
		t.Errorf("terminal=%s depth=%d want b/1 (약한 엣지 차단)", paths[0].Terminal.Pod, paths[0].Depth)
	}
}

// TestExtractImpactPaths_MaxDepth 는 maxDepth 도달 시 경로가 truncate 되는지 검증한다.
func TestExtractImpactPaths_MaxDepth(t *testing.T) {
	g := graphFromEdges(
		[3]interface{}{"b", "a", 0.9},
		[3]interface{}{"c", "b", 0.9},
		[3]interface{}{"d", "c", 0.9},
	)
	paths, _ := ExtractImpactPaths(g, 2, 0.5, 1024)
	if len(paths) != 1 {
		t.Fatalf("paths=%d want 1", len(paths))
	}
	if paths[0].Depth != 2 || paths[0].Terminal.Pod != "c" {
		t.Errorf("depth=%d terminal=%s want 2/c (maxDepth 트렁케이션)", paths[0].Depth, paths[0].Terminal.Pod)
	}
}

// TestRootSuspects_ReachAndCount 는 root 별 distinct 종착 victim 수 (reach) 와 경로 수가 정확히
// 집계되고 reach 내림차순 정렬되는지 검증한다.
func TestRootSuspects_ReachAndCount(t *testing.T) {
	// root a 가 b, c 두 terminal 로 (reach 2), root r 이 z 하나로 (reach 1).
	g := graphFromEdges(
		[3]interface{}{"b", "a", 0.9},
		[3]interface{}{"c", "a", 0.8},
		[3]interface{}{"z", "r", 0.7},
	)
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	roots := RootSuspects(paths)
	if len(roots) != 2 {
		t.Fatalf("roots=%d want 2", len(roots))
	}
	// reach 내림차순: a(2) 가 r(1) 보다 앞.
	if roots[0].Root.Pod != "a" || roots[0].Reach != 2 || roots[0].PathCount != 2 {
		t.Errorf("roots[0]=%s reach=%d pathCount=%d want a/2/2", roots[0].Root.Pod, roots[0].Reach, roots[0].PathCount)
	}
	if roots[1].Root.Pod != "r" || roots[1].Reach != 1 {
		t.Errorf("roots[1]=%s reach=%d want r/1", roots[1].Root.Pod, roots[1].Reach)
	}
}

// TestExtractImpactPaths_NetSourceFallback 은 순수 source (in-degree 0) 가 없는 순환 그래프에서
// net-source (out>in) 로 fallback 해 경로가 추출되고 RootKind 가 net_source 로 표시되는지 검증한다.
func TestExtractImpactPaths_NetSourceFallback(t *testing.T) {
	// a→b, b→c, c→a (3-순환) 에 a→x 를 더해 a 는 out=2/in=1 (net-source), 나머지는 out=in=1.
	// 순수 source 가 없어 a 가 net-source 근원이 된다.
	g := graphFromEdges(
		[3]interface{}{"b", "a", 0.9},
		[3]interface{}{"c", "b", 0.9},
		[3]interface{}{"a", "c", 0.9},
		[3]interface{}{"x", "a", 0.9},
	)
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	if len(paths) == 0 {
		t.Fatalf("paths=0 want >0 (net-source fallback 미동작)")
	}
	for _, p := range paths {
		if p.Root.Pod != "a" {
			t.Errorf("root=%s want a (net-source)", p.Root.Pod)
		}
		if p.RootKind != RootKindNetSource {
			t.Errorf("root_kind=%s want net_source", p.RootKind)
		}
	}
	roots := RootSuspects(paths)
	if len(roots) != 1 || roots[0].Kind != RootKindNetSource {
		t.Errorf("roots=%+v want 1 net_source", roots)
	}
}

// TestExtractImpactPaths_PureRootKind 는 순수 source 근원의 RootKind 가 source 인지 검증한다.
func TestExtractImpactPaths_PureRootKind(t *testing.T) {
	g := graphFromEdges([3]interface{}{"b", "a", 0.9})
	paths, _ := ExtractImpactPaths(g, 5, 0.5, 1024)
	if len(paths) != 1 || paths[0].RootKind != RootKindSource {
		t.Errorf("paths=%d kind=%v want 1 source", len(paths), paths[0].RootKind)
	}
}

// TestExtractImpactPaths_TruncatedFlag 는 maxPaths 캡 도달 시 truncated=true 가 반환되는지 검증한다.
func TestExtractImpactPaths_TruncatedFlag(t *testing.T) {
	// root a 가 b, c, d 로 분기 (3 경로). maxPaths=2 면 truncated.
	g := graphFromEdges(
		[3]interface{}{"b", "a", 0.9},
		[3]interface{}{"c", "a", 0.9},
		[3]interface{}{"d", "a", 0.9},
	)
	paths, truncated := ExtractImpactPaths(g, 5, 0.5, 2)
	if len(paths) != 2 || !truncated {
		t.Errorf("paths=%d truncated=%v want 2/true (maxPaths cap)", len(paths), truncated)
	}
	_, notTrunc := ExtractImpactPaths(g, 5, 0.5, 1024)
	if notTrunc {
		t.Errorf("truncated=true want false (cap 미도달)")
	}
}

// TestExtractImpactPaths_Deterministic 은 입력 순서가 달라도 경로 추출이 결정적인지 검증한다.
func TestExtractImpactPaths_Deterministic(t *testing.T) {
	a, _ := ExtractImpactPaths(graphFromEdges([3]interface{}{"c", "a", 0.8}, [3]interface{}{"b", "a", 0.9}), 5, 0.5, 1024)
	b, _ := ExtractImpactPaths(graphFromEdges([3]interface{}{"b", "a", 0.9}, [3]interface{}{"c", "a", 0.8}), 5, 0.5, 1024)
	if len(a) != len(b) {
		t.Fatalf("len mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Terminal.Pod != b[i].Terminal.Pod {
			t.Errorf("path[%d] terminal %s != %s (비결정적)", i, a[i].Terminal.Pod, b[i].Terminal.Pod)
		}
	}
}
