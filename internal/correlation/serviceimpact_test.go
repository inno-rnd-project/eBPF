package correlation

import "testing"

// workloadSeries 는 service-impact victim 입력 helper 다. src_namespace 와 src_workload 만 채워 pod
// 라벨이 없는 workload-level latency 시계열을 만든다. nodeSeries 는 crossnode_test.go 에 정의돼 있어
// 본 패키지에서 그대로 재사용한다.
func workloadSeries(ns, workload, metric string) LabeledSeries {
	return LabeledSeries{
		Metric: metric,
		Series: TimeSeries{Labels: map[string]string{
			"src_namespace": ns,
			"src_workload":  workload,
		}},
	}
}

// TestEnumerateServiceImpactPairs_NodePressureToWorkloadLatency 는 node 압박 suspect 와 workload
// latency victim 의 단일 방향 페어만 생성되는지 검증한다.
func TestEnumerateServiceImpactPairs_NodePressureToWorkloadLatency(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:cpu_pressure_score:5m"),
		workloadSeries("ns", "api", "workload:netobs_stage_latency_p99:5m"),
	}
	got := EnumerateServiceImpactPairs(items)
	if len(got) != 1 {
		t.Fatalf("페어 수=%d want 1", len(got))
	}
	p := got[0]
	if p.Key.SuspectNode != "n1" || p.Key.SuspectMetric != "node:cpu_pressure_score:5m" {
		t.Errorf("suspect=%s|%s want n1|node:cpu_pressure_score:5m", p.Key.SuspectNode, p.Key.SuspectMetric)
	}
	if p.Key.VictimNamespace != "ns" || p.Key.VictimWorkload != "api" || p.Key.VictimMetric != "workload:netobs_stage_latency_p99:5m" {
		t.Errorf("victim=%s/%s|%s want ns/api|workload:netobs_stage_latency_p99:5m", p.Key.VictimNamespace, p.Key.VictimWorkload, p.Key.VictimMetric)
	}
}

// TestEnumerateServiceImpactPairs_ExcludesPodAndNodeLatency 는 pod-level latency (src_pod 보유) 와
// node-level latency (src_namespace 없음) 가 victim 후보에서 제외되는지 검증한다. 세 layer 의 입력이
// 라벨 schema 로 분리되는 핵심 회귀 가드다.
func TestEnumerateServiceImpactPairs_ExcludesPodAndNodeLatency(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:cpu_pressure_score:5m"),
		// pod-level latency: src_pod 가 있어 workload victim 이 아니다.
		podSeries("n1", "ns", "pod-a", "histogram_quantile(0.99, ...latency...)"),
		// node-level latency: src_namespace 가 없어 victim 도 suspect 도 아니다 (latency).
		nodeSeries("n2", "node:netobs_pod_stage_latency_p99:5m"),
	}
	got := EnumerateServiceImpactPairs(items)
	if len(got) != 0 {
		t.Errorf("페어 수=%d want 0 (pod / node latency 가 workload victim 으로 잡힘)", len(got))
	}
}

// TestEnumerateServiceImpactPairs_ExcludesNodeLatencyAsSuspect 는 node-level latency 가 suspect
// 후보에서 제외되는지 검증한다. suspect 는 non-latency node 압박만 인정해야 한다.
func TestEnumerateServiceImpactPairs_ExcludesNodeLatencyAsSuspect(t *testing.T) {
	items := []LabeledSeries{
		nodeSeries("n1", "node:netobs_pod_stage_latency_p99:5m"),
		workloadSeries("ns", "api", "workload:netobs_stage_latency_p99:5m"),
	}
	got := EnumerateServiceImpactPairs(items)
	if len(got) != 0 {
		t.Errorf("페어 수=%d want 0 (node latency 가 suspect 로 잡힘)", len(got))
	}
}

// TestEnumerateServiceImpactPairs_CrossProductDeterministic 는 2 node * 2 workload 가 4 페어로
// cross-product 되고 (node, namespace, workload) 정렬 순서가 결정적인지 검증한다.
func TestEnumerateServiceImpactPairs_CrossProductDeterministic(t *testing.T) {
	items := []LabeledSeries{
		workloadSeries("ns", "web", "workload:netobs_stage_latency_p99:5m"),
		nodeSeries("n2", "node:cpu_pressure_score:5m"),
		workloadSeries("ns", "api", "workload:netobs_stage_latency_p99:5m"),
		nodeSeries("n1", "node:cpu_pressure_score:5m"),
	}
	got := EnumerateServiceImpactPairs(items)
	if len(got) != 4 {
		t.Fatalf("페어 수=%d want 4", len(got))
	}
	// 정렬: suspect (n1, n2) 외곽, victim (api, web) 내곽.
	want := []struct{ node, workload string }{
		{"n1", "api"}, {"n1", "web"}, {"n2", "api"}, {"n2", "web"},
	}
	for i, w := range want {
		if got[i].Key.SuspectNode != w.node || got[i].Key.VictimWorkload != w.workload {
			t.Errorf("got[%d]=%s/%s want %s/%s", i, got[i].Key.SuspectNode, got[i].Key.VictimWorkload, w.node, w.workload)
		}
	}
}

// serviceImpactResult 는 SelectTopNServiceImpact 테스트 입력 helper 다.
func serviceImpactResult(suspectNode, suspectMetric, victimNS, victimWorkload, victimMetric string, score float64) CorrelationResult {
	return CorrelationResult{
		IsServiceImpact: true,
		ServiceImpactPair: ServiceImpactPairKey{
			SuspectNode:     suspectNode,
			SuspectMetric:   suspectMetric,
			VictimNamespace: victimNS,
			VictimWorkload:  victimWorkload,
			VictimMetric:    victimMetric,
		},
		MaxAbsValue: score,
		Status:      StatusOK,
		SampleCount: 60,
	}
}

const (
	siLatency = "workload:netobs_stage_latency_p99:5m"
	siCPU     = "node:cpu_pressure_score:5m"
	siMem     = "node:memory_pressure_score:5m"
)

// TestSelectTopNServiceImpact_FiltersServiceImpactOnly 는 IsServiceImpact=false 항목 (pod / cross-node
// 결과) 이 제외되는지 검증한다.
func TestSelectTopNServiceImpact_FiltersServiceImpactOnly(t *testing.T) {
	results := []CorrelationResult{
		serviceImpactResult("n1", siCPU, "ns", "api", siLatency, 0.8),
		{Status: StatusOK, MaxAbsValue: 0.9, IsCrossNode: true, NodePair: NodePairKey{SrcNode: "n1", DstNode: "n2"}},
		{Status: StatusOK, MaxAbsValue: 0.95, Pair: PairKey{}},
	}
	got := SelectTopNServiceImpact(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (pod / cross-node 결과가 포함됨)", len(got))
	}
	if got[0].VictimWorkload != "api" || got[0].SuspectNode != "n1" {
		t.Errorf("victim=%s suspect=%s want api/n1", got[0].VictimWorkload, got[0].SuspectNode)
	}
	if got[0].Dimension != DimensionCPU {
		t.Errorf("dimension=%s want cpu", got[0].Dimension)
	}
}

// TestSelectTopNServiceImpact_GroupsByVictimWorkloadDimension 은 (victim_workload, dimension) 그룹별
// rank 가 독립 부여되는지 검증한다.
func TestSelectTopNServiceImpact_GroupsByVictimWorkloadDimension(t *testing.T) {
	results := []CorrelationResult{
		// victim=api cpu: 두 suspect node
		serviceImpactResult("n1", siCPU, "ns", "api", siLatency, 0.9),
		serviceImpactResult("n2", siCPU, "ns", "api", siLatency, 0.7),
		// victim=api memory: 한 suspect node
		serviceImpactResult("n1", siMem, "ns", "api", siLatency, 0.6),
	}
	got := SelectTopNServiceImpact(results, 10)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	var foundRank1Cpu bool
	for _, s := range got {
		if s.VictimWorkload == "api" && s.Dimension == DimensionCPU && s.Rank == 1 {
			if s.Score != 0.9 || s.SuspectNode != "n1" {
				t.Errorf("(api, cpu) rank=1 score=%v suspect=%s want 0.9/n1", s.Score, s.SuspectNode)
			}
			foundRank1Cpu = true
		}
	}
	if !foundRank1Cpu {
		t.Errorf("(api, cpu) rank=1 entry 누락")
	}
}

// TestSelectTopNServiceImpact_DedupsByKey 는 같은 (victim_namespace, victim_workload, suspect_node,
// dimension) 키의 multiple candidate 가 max score 1개로 dedup 되는지 검증한다.
func TestSelectTopNServiceImpact_DedupsByKey(t *testing.T) {
	results := []CorrelationResult{
		serviceImpactResult("n1", siCPU, "ns", "api", siLatency, 0.85),
		serviceImpactResult("n1", siCPU, "ns", "api", siLatency, 0.6),
	}
	got := SelectTopNServiceImpact(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (dedup 미동작)", len(got))
	}
	if got[0].Score != 0.85 {
		t.Errorf("score=%v want 0.85 (max dedup 정책 위배)", got[0].Score)
	}
}

// TestSelectTopNServiceImpact_TopNCut 는 topN 컷이 그룹별로 적용되는지 검증한다.
func TestSelectTopNServiceImpact_TopNCut(t *testing.T) {
	results := []CorrelationResult{
		serviceImpactResult("n1", siCPU, "ns", "api", siLatency, 0.9),
		serviceImpactResult("n2", siCPU, "ns", "api", siLatency, 0.8),
		serviceImpactResult("n3", siCPU, "ns", "api", siLatency, 0.7),
	}
	got := SelectTopNServiceImpact(results, 2)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (topN 컷 미동작)", len(got))
	}
	if got[0].Rank != 1 || got[0].SuspectNode != "n1" || got[1].Rank != 2 || got[1].SuspectNode != "n2" {
		t.Errorf("rank/suspect 순서 오류: %+v", got)
	}
}

// TestConfigPlannedQueries_Dedup 은 cross-node 와 service-impact 가 공유하는 node 압박 score 가 한
// 번만 등장하는지 (dedup) 검증한다.
func TestConfigPlannedQueries_Dedup(t *testing.T) {
	cfg := DefaultConfig()
	queries := cfg.PlannedQueries()
	seen := make(map[string]int)
	for _, q := range queries {
		seen[q]++
	}
	for q, n := range seen {
		if n != 1 {
			t.Errorf("query %q 가 %d 회 중복 (dedup 미동작)", q, n)
		}
	}
	// service-impact victim rule 과 cross-node 전용 node latency 가 모두 포함돼야 한다.
	if seen["workload:netobs_stage_latency_p99:5m"] != 1 {
		t.Errorf("workload latency rule 누락")
	}
	if seen["node:cpu_pressure_score:5m"] != 1 {
		t.Errorf("공유 node 압박 score 가 1회로 정규화되지 않음")
	}
}

// TestConfigPlannedQueries_RespectsToggles 는 토글 off 시 해당 layer query 가 빠지는지 검증한다.
// node 입도 입력 (node:netobs_pod_stage_latency_p99:5m) 은 cross-node 와 cross-level (#149) 둘 다의
// 입력이라 두 토글을 모두 꺼야 빠진다.
func TestConfigPlannedQueries_RespectsToggles(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CrossNodeEnabled = false
	cfg.ServiceImpactEnabled = false
	cfg.CrossLevelEnabled = false
	queries := cfg.PlannedQueries()
	for _, q := range queries {
		if q == "workload:netobs_stage_latency_p99:5m" || q == "node:netobs_pod_stage_latency_p99:5m" {
			t.Errorf("토글 off 인데 layer query %q 가 포함됨", q)
		}
	}
}
