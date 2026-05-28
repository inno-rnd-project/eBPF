package correlation

import (
	"testing"
)

// nodeSeries 는 테스트 입력 helper 다. node 라벨 만 있고 src_namespace / src_pod 가 비어 있는
// node-level 시계열 을 만든다.
func nodeSeries(node, metric string) LabeledSeries {
	return LabeledSeries{
		Metric: metric,
		Series: TimeSeries{Labels: map[string]string{"node": node}},
	}
}

// podSeries 는 EnumerateNodePairs 가 pod-level 시계열 을 입력에서 자동 제외 하는지 검증 하기 위한
// helper 다.
func podSeries(node, ns, pod, metric string) LabeledSeries {
	return LabeledSeries{
		Metric: metric,
		Series: TimeSeries{Labels: map[string]string{
			"node":          node,
			"src_namespace": ns,
			"src_pod":       pod,
		}},
	}
}

// TestEnumerateNodePairs_ExcludesSameNode 는 victim_node 와 suspect_node 가 같은 페어 가 자동 제외
// 되는지 검증한다. cross-node 분석 의 핵심 회귀 가드다.
func TestEnumerateNodePairs_ExcludesSameNode(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:cpu_pressure_score:5m"),
		nodeSeries("n1", "node:netobs_pod_stage_latency_p99:5m"),
	}
	got := EnumerateNodePairs(items)
	if len(got) != 0 {
		t.Errorf("single-node 입력에서 페어 %d개 생성됨 (cross-node 가드 미동작)", len(got))
	}
}

// TestEnumerateNodePairs_OnlyPressureToLatency 는 noisy neighbor 모델의 단일 방향 (Src=non-latency
// suspect, Dst=latency victim) 페어만 생성되는지 검증한다. 반대 방향 (latency → pressure) 과 같은
// 종류 페어 (pressure → pressure, latency → latency) 는 사전 필터로 자동 제외되어 Granger 인과성
// 검정의 행렬 연산 비용을 회피한다.
func TestEnumerateNodePairs_OnlyPressureToLatency(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:cpu_pressure_score:5m"),
		nodeSeries("n2", "node:netobs_pod_stage_latency_p99:5m"),
	}
	got := EnumerateNodePairs(items)
	// 유효 페어는 (n1 pressure → n2 latency) 단일 방향 1개뿐이다.
	if len(got) != 1 {
		t.Fatalf("페어 수=%d want 1 (pre-filter 미동작)", len(got))
	}
	p := got[0]
	if p.Key.SrcNode != "n1" || p.Key.SrcMetric != "node:cpu_pressure_score:5m" {
		t.Errorf("Src=%s|%s want n1|node:cpu_pressure_score:5m", p.Key.SrcNode, p.Key.SrcMetric)
	}
	if p.Key.DstNode != "n2" || p.Key.DstMetric != "node:netobs_pod_stage_latency_p99:5m" {
		t.Errorf("Dst=%s|%s want n2|node:netobs_pod_stage_latency_p99:5m", p.Key.DstNode, p.Key.DstMetric)
	}
}

// TestEnumerateNodePairs_ExcludesReverseDirection 은 latency → pressure 의 역방향 페어가 사전 필터로
// 제외되는지 검증한다. EnumerateNodePairs가 reverse 방향까지 생성하면 SelectTopNCrossNode에서 다시
// 걸러지므로 비용 낭비다.
func TestEnumerateNodePairs_ExcludesReverseDirection(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:netobs_pod_stage_latency_p99:5m"),
		nodeSeries("n2", "node:cpu_pressure_score:5m"),
	}
	got := EnumerateNodePairs(items)
	if len(got) != 1 {
		t.Fatalf("페어 수=%d want 1", len(got))
	}
	if isLatencyMetric(got[0].Key.SrcMetric) {
		t.Errorf("Src=%s 가 latency 메트릭임 (reverse direction 페어가 enumerate됨)", got[0].Key.SrcMetric)
	}
}

// TestEnumerateNodePairs_ExcludesPodLevelSeries 는 pod 라벨이 채워진 시계열이 자동 제외되는지
// 검증한다. pod-level 분석 (EnumeratePairs) 과의 입력 중복 차단 가드다.
func TestEnumerateNodePairs_ExcludesPodLevelSeries(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:cpu_pressure_score:5m"),
		podSeries("n2", "ns", "pod-a", "pod:cpu_throttle_score:5m"),
	}
	got := EnumerateNodePairs(items)
	if len(got) != 0 {
		t.Errorf("pod-level 시계열 이 cross-node 페어에 포함됨 (len=%d)", len(got))
	}
}

// TestEnumerateNodePairs_SkipsPressureToPressure 는 두 노드에 같은 pressure 메트릭 (둘 다 non-
// latency) 의 시계열이 있을 때 본 페어가 자동 제외되는지 검증한다. noisy neighbor 모델의 단일 방향
// (pressure → latency) 정책 회귀 가드다.
func TestEnumerateNodePairs_SkipsPressureToPressure(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:cpu_pressure_score:5m"),
		nodeSeries("n2", "node:cpu_pressure_score:5m"),
	}
	got := EnumerateNodePairs(items)
	if len(got) != 0 {
		t.Errorf("pressure → pressure 페어가 생성됨 (len=%d)", len(got))
	}
}

// crossNodeResult 는 SelectTopNCrossNode 테스트의 입력 helper 다. IsCrossNode=true 와 NodePair 를
// 함께 채워 본 함수의 분기 필터 가 정상 동작 하는지 검증할 수 있게 한다.
func crossNodeResult(srcNode, srcMetric, dstNode, dstMetric string, score float64) CorrelationResult {
	return CorrelationResult{
		IsCrossNode: true,
		NodePair: NodePairKey{
			SrcNode:   srcNode,
			SrcMetric: srcMetric,
			DstNode:   dstNode,
			DstMetric: dstMetric,
		},
		MaxAbsValue: score,
		Status:      StatusOK,
		SampleCount: 60,
	}
}

// TestSelectTopNCrossNode_FiltersCrossNodeOnly 는 IsCrossNode=false 인 항목 이 결과 에서 제외 되는지
// 검증한다. pod-level 결과 와 의 분리 회귀 가드 다.
func TestSelectTopNCrossNode_FiltersCrossNodeOnly(t *testing.T) {
	results := []CorrelationResult{
		crossNodeResult("n1", "node:cpu_pressure_score:5m", "n2", "node:netobs_pod_stage_latency_p99:5m", 0.8),
		{Status: StatusOK, MaxAbsValue: 0.9, Pair: PairKey{}},
	}
	got := SelectTopNCrossNode(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (pod-level 결과 가 포함됨)", len(got))
	}
	if got[0].VictimNode != "n2" || got[0].SuspectNode != "n1" {
		t.Errorf("victim=%s suspect=%s want n2/n1", got[0].VictimNode, got[0].SuspectNode)
	}
}

// TestSelectTopNCrossNode_GroupsByVictimNodeDimension 은 (victim_node, dimension) 그룹별 rank 가
// 독립 부여 되는지 검증한다.
func TestSelectTopNCrossNode_GroupsByVictimNodeDimension(t *testing.T) {
	results := []CorrelationResult{
		// victim=n2 cpu: 두 개 candidate
		crossNodeResult("n1", "node:cpu_pressure_score:5m", "n2", "node:netobs_pod_stage_latency_p99:5m", 0.9),
		crossNodeResult("n3", "node:cpu_pressure_score:5m", "n2", "node:netobs_pod_stage_latency_p99:5m", 0.7),
		// victim=n3 memory: 한 개 candidate
		crossNodeResult("n1", "node:memory_pressure_score:5m", "n3", "node:netobs_pod_stage_latency_p99:5m", 0.6),
	}
	got := SelectTopNCrossNode(results, 10)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	// (n2, cpu) 그룹 의 rank 1 이 score=0.9 인지 확인
	var foundRank1Cpu bool
	for _, n := range got {
		if n.VictimNode == "n2" && n.Dimension == DimensionCPU && n.Rank == 1 {
			if n.Score != 0.9 {
				t.Errorf("(n2, cpu) rank=1 score=%v want 0.9", n.Score)
			}
			foundRank1Cpu = true
		}
	}
	if !foundRank1Cpu {
		t.Errorf("(n2, cpu) rank=1 entry 누락")
	}
}

// TestSelectTopNCrossNode_DedupsByVictimSuspectDimension 은 같은 (victim_node, suspect_node,
// dimension) 키 의 multiple candidate 가 max score 1개 로 dedup 되는지 검증한다.
func TestSelectTopNCrossNode_DedupsByVictimSuspectDimension(t *testing.T) {
	results := []CorrelationResult{
		// 같은 (n2, n1, cpu) 키 의 두 candidate. max=0.85 만 채택 되어야 한다.
		crossNodeResult("n1", "node:cpu_pressure_score:5m", "n2", "node:netobs_pod_stage_latency_p99:5m", 0.85),
		crossNodeResult("n1", "node:cpu_pressure_score:5m", "n2", "node:netobs_pod_stage_latency_p99:5m", 0.6),
	}
	got := SelectTopNCrossNode(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (dedup 미동작)", len(got))
	}
	if got[0].Score != 0.85 {
		t.Errorf("score=%v want 0.85 (max dedup 정책 위배)", got[0].Score)
	}
}
