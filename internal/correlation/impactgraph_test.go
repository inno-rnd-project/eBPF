package correlation

import "testing"

func ngEdge(victimPod, suspectPod string, dim ResourceDimension, sig VictimSignal, score float64) NoisyNeighbor {
	return NoisyNeighbor{
		Victim:       PodIdentity{Namespace: "ns", Pod: victimPod, PodUID: "u-" + victimPod},
		Suspect:      PodIdentity{Namespace: "ns", Pod: suspectPod, PodUID: "u-" + suspectPod},
		Dimension:    dim,
		VictimSignal: sig,
		Score:        score,
		LagSteps:     1,
	}
}

// TestBuildImpactGraph_NodesAndEdges 는 NoisyNeighbor 가 suspect → victim 엣지로, 등장 pod 가 정점으로
// 구성되고 out / in degree 가 정확히 누적되는지 검증한다.
func TestBuildImpactGraph_NodesAndEdges(t *testing.T) {
	// a → b, a → c, b → c (b 는 victim 이자 suspect 인 다단계 중간 노드)
	neighbors := []NoisyNeighbor{
		ngEdge("b", "a", DimensionCPU, SignalLatency, 0.9),
		ngEdge("c", "a", DimensionMemory, SignalLatency, 0.7),
		ngEdge("c", "b", DimensionCPU, SignalThroughput, 0.6),
	}
	g := BuildImpactGraph(neighbors)

	if len(g.Edges) != 3 {
		t.Fatalf("edges=%d want 3", len(g.Edges))
	}
	if len(g.Nodes) != 3 {
		t.Fatalf("nodes=%d want 3 (a, b, c)", len(g.Nodes))
	}
	deg := map[string][2]int{} // pod -> [out, in]
	for _, n := range g.Nodes {
		deg[n.Pod] = [2]int{n.OutDegree, n.InDegree}
	}
	// a: suspect 2회 (out=2, in=0)
	if deg["a"] != [2]int{2, 0} {
		t.Errorf("a degree=%v want [2 0]", deg["a"])
	}
	// b: suspect 1회 + victim 1회 (out=1, in=1) — 다단계 중간 노드
	if deg["b"] != [2]int{1, 1} {
		t.Errorf("b degree=%v want [1 1]", deg["b"])
	}
	// c: victim 2회 (out=0, in=2)
	if deg["c"] != [2]int{0, 2} {
		t.Errorf("c degree=%v want [0 2]", deg["c"])
	}
}

// TestBuildImpactGraph_EdgeAttributes 는 엣지가 dimension / victim_signal / score 등 NoisyNeighbor
// 속성을 그대로 보존하는지 검증한다.
func TestBuildImpactGraph_EdgeAttributes(t *testing.T) {
	g := BuildImpactGraph([]NoisyNeighbor{ngEdge("b", "a", DimensionCPU, SignalThroughput, 0.85)})
	if len(g.Edges) != 1 {
		t.Fatalf("edges=%d want 1", len(g.Edges))
	}
	e := g.Edges[0]
	if e.Suspect.Pod != "a" || e.Victim.Pod != "b" {
		t.Errorf("edge=%s->%s want a->b", e.Suspect.Pod, e.Victim.Pod)
	}
	if e.Dimension != DimensionCPU || e.VictimSignal != SignalThroughput || e.Score != 0.85 {
		t.Errorf("edge attrs dim=%s signal=%s score=%v", e.Dimension, e.VictimSignal, e.Score)
	}
}

// TestBuildImpactGraph_Deterministic 은 입력 순서가 달라도 정점 / 엣지가 결정적으로 정렬되는지
// 검증한다.
func TestBuildImpactGraph_Deterministic(t *testing.T) {
	a := BuildImpactGraph([]NoisyNeighbor{
		ngEdge("c", "b", DimensionCPU, SignalLatency, 0.6),
		ngEdge("b", "a", DimensionCPU, SignalLatency, 0.9),
	})
	b := BuildImpactGraph([]NoisyNeighbor{
		ngEdge("b", "a", DimensionCPU, SignalLatency, 0.9),
		ngEdge("c", "b", DimensionCPU, SignalLatency, 0.6),
	})
	if len(a.Nodes) != len(b.Nodes) || len(a.Edges) != len(b.Edges) {
		t.Fatalf("size mismatch")
	}
	for i := range a.Nodes {
		if a.Nodes[i] != b.Nodes[i] {
			t.Errorf("node[%d] %+v != %+v (정렬 비결정적)", i, a.Nodes[i], b.Nodes[i])
		}
	}
	for i := range a.Edges {
		if a.Edges[i].Suspect.Pod != b.Edges[i].Suspect.Pod || a.Edges[i].Victim.Pod != b.Edges[i].Victim.Pod {
			t.Errorf("edge[%d] 순서 비결정적", i)
		}
	}
}

// TestBuildImpactGraph_DeterministicByPodUID 는 동명 pod 가 UID 만 다르게 공존할 때 (재생성 전후)
// 엣지 정렬이 PodUID 까지 비교해 결정적인지 검증한다. namespace / pod / dimension / signal 이 같고
// UID 만 다른 두 엣지가 입력 순서와 무관하게 항상 같은 순서로 정렬돼야 한다.
func TestBuildImpactGraph_DeterministicByPodUID(t *testing.T) {
	mk := func(victimUID, suspectUID string) NoisyNeighbor {
		return NoisyNeighbor{
			Victim:       PodIdentity{Namespace: "ns", Pod: "v", PodUID: victimUID},
			Suspect:      PodIdentity{Namespace: "ns", Pod: "s", PodUID: suspectUID},
			Dimension:    DimensionCPU,
			VictimSignal: SignalLatency,
			Score:        0.8,
		}
	}
	// suspect UID 만 다른 두 엣지를 서로 다른 입력 순서로 빌드.
	g1 := BuildImpactGraph([]NoisyNeighbor{mk("uv", "s2"), mk("uv", "s1")})
	g2 := BuildImpactGraph([]NoisyNeighbor{mk("uv", "s1"), mk("uv", "s2")})
	if len(g1.Edges) != 2 || len(g2.Edges) != 2 {
		t.Fatalf("edges g1=%d g2=%d want 2/2", len(g1.Edges), len(g2.Edges))
	}
	for i := range g1.Edges {
		if g1.Edges[i].Suspect.PodUID != g2.Edges[i].Suspect.PodUID {
			t.Errorf("edge[%d] suspect uid g1=%s g2=%s (UID 정렬 비결정적)", i, g1.Edges[i].Suspect.PodUID, g2.Edges[i].Suspect.PodUID)
		}
	}
	// UID 사전순 (s1 < s2) 이 보장돼야 한다.
	if g1.Edges[0].Suspect.PodUID != "s1" || g1.Edges[1].Suspect.PodUID != "s2" {
		t.Errorf("정렬 순서=%s,%s want s1,s2", g1.Edges[0].Suspect.PodUID, g1.Edges[1].Suspect.PodUID)
	}
}

// TestBuildImpactGraph_Empty 는 빈 입력에서 빈 그래프를 반환하는지 검증한다.
func TestBuildImpactGraph_Empty(t *testing.T) {
	g := BuildImpactGraph(nil)
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Errorf("empty 입력에서 nodes=%d edges=%d want 0/0", len(g.Nodes), len(g.Edges))
	}
}
