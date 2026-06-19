package correlation

import "testing"

// podMetricSeries 는 cross-level 테스트용 pod-level 입력 helper 다. node + src_namespace + src_pod
// 라벨을 채운다. nodeSeries 는 crossnode_test.go 에 정의돼 있어 재사용한다.
func podMetricSeries(node, ns, pod, metric string) LabeledSeries {
	return LabeledSeries{
		Metric: metric,
		Series: TimeSeries{Labels: map[string]string{
			"node":          node,
			"src_namespace": ns,
			"src_pod":       pod,
			"src_pod_uid":   "uid-" + pod,
		}},
	}
}

const (
	clNodePress = "node:cpu_pressure_score:5m"
	clNodeLat   = "node:netobs_pod_stage_latency_p99:5m"
	clPodPress  = "pod:cpu_throttle_score:5m"
	clPodLat    = "histogram_quantile(0.99, ... netobs_pod_stage_latency_labeled_seconds_bucket ...)"
)

// TestEnumerateCrossLevelPairs_BothDirectionsSameNode 는 동일 node 에서 node→pod 와 pod→node 두 방향
// 페어가 생성되고 다른 node 와는 섞이지 않는지 검증한다.
func TestEnumerateCrossLevelPairs_BothDirectionsSameNode(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", clNodePress),
		nodeSeries("n1", clNodeLat),
		podMetricSeries("n1", "ns", "pod-a", clPodPress),
		podMetricSeries("n1", "ns", "pod-a", clPodLat),
	}
	got := EnumerateCrossLevelPairs(items, nil)
	// node→pod: nodePress(n1) × podLat(pod-a) = 1
	// pod→node: podPress(pod-a) × nodeLat(n1) = 1
	if len(got) != 2 {
		t.Fatalf("페어 수=%d want 2", len(got))
	}
	var n2p, p2n bool
	for _, p := range got {
		if p.Key.Node != "n1" {
			t.Errorf("node=%s want n1", p.Key.Node)
		}
		switch p.Key.Direction {
		case DirectionNodeToPod:
			n2p = true
			if p.Key.SrcMetric != clNodePress || p.Key.DstMetric != clPodLat {
				t.Errorf("node_to_pod src/dst=%s/%s", p.Key.SrcMetric, p.Key.DstMetric)
			}
			if p.Key.Pod != "pod-a" {
				t.Errorf("node_to_pod pod=%s want pod-a", p.Key.Pod)
			}
		case DirectionPodToNode:
			p2n = true
			if p.Key.SrcMetric != clPodPress || p.Key.DstMetric != clNodeLat {
				t.Errorf("pod_to_node src/dst=%s/%s", p.Key.SrcMetric, p.Key.DstMetric)
			}
		}
	}
	if !n2p || !p2n {
		t.Errorf("방향 누락: node_to_pod=%v pod_to_node=%v", n2p, p2n)
	}
}

// TestEnumerateCrossLevelPairs_ExcludesCrossNode 는 서로 다른 node 의 node↔pod 가 섞이지 않는지 (이슈
// 비목표) 검증한다.
func TestEnumerateCrossLevelPairs_ExcludesCrossNode(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", clNodePress),
		podMetricSeries("n2", "ns", "pod-b", clPodLat),
	}
	got := EnumerateCrossLevelPairs(items, nil)
	if len(got) != 0 {
		t.Errorf("페어 수=%d want 0 (다른 node 의 node↔pod 가 생성됨)", len(got))
	}
}

// TestEnumerateCrossLevelPairs_AllowList 는 allow-list 에 없는 namespace 의 pod 가 양방향 모두에서
// 제외되는지 검증한다.
func TestEnumerateCrossLevelPairs_AllowList(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", clNodePress),
		nodeSeries("n1", clNodeLat),
		podMetricSeries("n1", "allowed", "pod-a", clPodPress),
		podMetricSeries("n1", "allowed", "pod-a", clPodLat),
		podMetricSeries("n1", "blocked", "pod-b", clPodPress),
		podMetricSeries("n1", "blocked", "pod-b", clPodLat),
	}
	got := EnumerateCrossLevelPairs(items, []string{"allowed"})
	for _, p := range got {
		if p.Key.PodNamespace != "allowed" {
			t.Errorf("allow-list 밖 namespace=%s 의 페어가 생성됨", p.Key.PodNamespace)
		}
	}
	// allowed pod-a 만: node→pod 1 + pod→node 1 = 2
	if len(got) != 2 {
		t.Fatalf("페어 수=%d want 2 (allow-list 적용)", len(got))
	}
}

// TestEnumerateCrossLevelPairs_NoNodeSeriesNoCrossLevel 은 node-level 시계열이 없으면 cross-level
// 페어가 0 인지 검증한다 (pod-level 만으로는 cross-level 이 성립하지 않음).
func TestEnumerateCrossLevelPairs_NoNodeSeriesNoCrossLevel(t *testing.T) {
	items := []LabeledSeries{
		podMetricSeries("n1", "ns", "pod-a", clPodPress),
		podMetricSeries("n1", "ns", "pod-a", clPodLat),
	}
	got := EnumerateCrossLevelPairs(items, nil)
	if len(got) != 0 {
		t.Errorf("페어 수=%d want 0 (node 시계열 부재인데 cross-level 생성)", len(got))
	}
}

// crossLevelResult 는 SelectTopNCrossLevel 테스트 입력 helper 다.
func crossLevelResult(node string, dir CrossLevelDirection, ns, pod, suspectMetric, victimMetric string, score float64) CorrelationResult {
	return CorrelationResult{
		IsCrossLevel: true,
		CrossLevelPair: CrossLevelPairKey{
			Node:         node,
			Direction:    dir,
			PodNamespace: ns,
			Pod:          pod,
			PodUID:       "uid-" + pod,
			SrcMetric:    suspectMetric,
			DstMetric:    victimMetric,
		},
		MaxAbsValue: score,
		Status:      StatusOK,
		SampleCount: 60,
	}
}

// TestSelectTopNCrossLevel_FiltersCrossLevelOnly 는 IsCrossLevel=false 항목 (다른 layer 결과) 이
// 제외되는지 검증한다.
func TestSelectTopNCrossLevel_FiltersCrossLevelOnly(t *testing.T) {
	results := []CorrelationResult{
		crossLevelResult("n1", DirectionNodeToPod, "ns", "pod-a", clNodePress, clPodLat, 0.8),
		{Status: StatusOK, MaxAbsValue: 0.9, IsCrossNode: true, NodePair: NodePairKey{SrcNode: "n1", DstNode: "n2"}},
		{Status: StatusOK, MaxAbsValue: 0.95, Pair: PairKey{}},
	}
	got := SelectTopNCrossLevel(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (다른 layer 결과가 포함됨)", len(got))
	}
	if got[0].Pod != "pod-a" || got[0].Direction != DirectionNodeToPod || got[0].Dimension != DimensionCPU {
		t.Errorf("pod=%s dir=%s dim=%s", got[0].Pod, got[0].Direction, got[0].Dimension)
	}
}

// TestSelectTopNCrossLevel_GroupsByNodeDirectionDimension 은 (node, direction, dimension) 그룹별 rank
// 가 독립 부여되고 pod 가 점수 내림차순으로 정렬되는지 검증한다.
func TestSelectTopNCrossLevel_GroupsByNodeDirectionDimension(t *testing.T) {
	results := []CorrelationResult{
		crossLevelResult("n1", DirectionPodToNode, "ns", "pod-a", clPodPress, clNodeLat, 0.7),
		crossLevelResult("n1", DirectionPodToNode, "ns", "pod-b", clPodPress, clNodeLat, 0.9),
		// 다른 방향 그룹 (node→pod) 은 독립 rank.
		crossLevelResult("n1", DirectionNodeToPod, "ns", "pod-a", clNodePress, clPodLat, 0.5),
	}
	got := SelectTopNCrossLevel(results, 10)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	var found bool
	for _, c := range got {
		if c.Direction == DirectionPodToNode && c.Rank == 1 {
			if c.Pod != "pod-b" || c.Score != 0.9 {
				t.Errorf("pod_to_node rank1 pod=%s score=%v want pod-b/0.9", c.Pod, c.Score)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("pod_to_node rank1 누락")
	}
}

// TestSelectTopNCrossLevel_DedupsByKey 는 같은 (node, direction, dimension, pod) 키의 multiple
// candidate (한 pod 이 cpu_throttle 와 host_compute_stall 둘 다 cpu) 가 max score 1개로 dedup 되는지
// 검증한다.
func TestSelectTopNCrossLevel_DedupsByKey(t *testing.T) {
	results := []CorrelationResult{
		crossLevelResult("n1", DirectionPodToNode, "ns", "pod-a", "pod:cpu_throttle_score:5m", clNodeLat, 0.6),
		crossLevelResult("n1", DirectionPodToNode, "ns", "pod-a", "pod:host_compute_stall_score:5m", clNodeLat, 0.85),
	}
	got := SelectTopNCrossLevel(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (dedup 미동작)", len(got))
	}
	if got[0].Score != 0.85 {
		t.Errorf("score=%v want 0.85 (max dedup 위배)", got[0].Score)
	}
}

// TestSelectTopNCrossLevel_TopNCut 은 그룹별 topN 컷이 적용되는지 검증한다.
func TestSelectTopNCrossLevel_TopNCut(t *testing.T) {
	results := []CorrelationResult{
		crossLevelResult("n1", DirectionNodeToPod, "ns", "pod-a", clNodePress, clPodLat, 0.9),
		crossLevelResult("n1", DirectionNodeToPod, "ns", "pod-b", clNodePress, clPodLat, 0.8),
		crossLevelResult("n1", DirectionNodeToPod, "ns", "pod-c", clNodePress, clPodLat, 0.7),
	}
	got := SelectTopNCrossLevel(results, 2)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (topN 컷 미동작)", len(got))
	}
	if got[0].Rank != 1 || got[0].Pod != "pod-a" || got[1].Rank != 2 || got[1].Pod != "pod-b" {
		t.Errorf("rank/pod 순서 오류: %+v", got)
	}
}

// TestConfigPlannedQueries_CrossLevelPullsNodeMetrics 는 CrossNodeEnabled=false 라도 CrossLevelEnabled
// 면 node 입도 입력 (CrossNodeMetrics) 이 fetch 셋에 포함되는지 검증한다.
func TestConfigPlannedQueries_CrossLevelPullsNodeMetrics(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CrossNodeEnabled = false
	cfg.ServiceImpactEnabled = false
	cfg.CrossLevelEnabled = true
	queries := cfg.PlannedQueries()
	seen := make(map[string]struct{})
	for _, q := range queries {
		seen[q] = struct{}{}
	}
	if _, ok := seen["node:netobs_pod_stage_latency_p99:5m"]; !ok {
		t.Errorf("cross-level 활성인데 node latency 입력 누락")
	}
	if _, ok := seen["node:cpu_pressure_score:5m"]; !ok {
		t.Errorf("cross-level 활성인데 node 압박 입력 누락")
	}
}
