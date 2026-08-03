package correlation

import (
	"sort"
	"testing"
)

// ls 는 테스트용 LabeledSeries 짧은 생성자다. metric / node / namespace / pod 만 지정하고 시계열
// 데이터는 의미가 없으니 빈 슬라이스로 둔다. UID 는 namespace + pod 기준의 deterministic stub 으로
// PairKey assert 에서도 사용 가능하다.
func ls(metric, node, ns, pod string) LabeledSeries {
	return LabeledSeries{
		Metric: metric,
		Series: TimeSeries{
			Labels: map[string]string{
				"node":          node,
				"src_namespace": ns,
				"src_pod":       pod,
				"src_pod_uid":   "uid-" + ns + "-" + pod,
			},
		},
	}
}

// pairKeys 는 EnumeratePairs 결과를 (src→dst) 비교용 정렬된 문자열 슬라이스로 변환한다. 결정적 비교
// 를 위해 정렬한 결과로 검증한다.
func pairKeys(ps []Pair) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Key.SrcMetric + "@" + p.Key.SrcNamespace + "/" + p.Key.SrcPod +
			" -> " + p.Key.DstMetric + "@" + p.Key.DstNamespace + "/" + p.Key.DstPod
	}
	sort.Strings(out)
	return out
}

// TestEnumeratePairsSameNodeOnly 는 동일 노드에 있는 Pod 들끼리만 pair 가 생성되고 cross-node 는
// 제외되는지 검증한다. 본 정책으로 cross-product N^2 을 노드 단위 N_node^2 로 통제해 cardinality
// 폭발을 막는다.
func TestEnumeratePairsSameNodeOnly(t *testing.T) {
	items := []LabeledSeries{
		ls("pod:cpu_throttle_score:5m", "node-a", "ns", "pod-1"),
		ls("pod:stage_latency_p99:5m", "node-a", "ns", "pod-2"),
		ls("pod:stage_latency_p99:5m", "node-b", "ns", "pod-3"),
	}
	got := pairKeys(EnumeratePairs(items))
	want := []string{
		"pod:cpu_throttle_score:5m@ns/pod-1 -> pod:stage_latency_p99:5m@ns/pod-2",
	}
	if !equalStringSlices(got, want) {
		t.Errorf("\n got=%v\nwant=%v (node-b pod must be excluded)", got, want)
	}
}

// TestEnumeratePairsExcludesSelf 는 동일 (metric, namespace, pod) 시계열이 자기 자신과 페어 되지
// 않는지 검증한다.
func TestEnumeratePairsExcludesSelf(t *testing.T) {
	items := []LabeledSeries{
		ls("m1", "node-a", "ns", "pod-1"),
		ls("m1", "node-a", "ns", "pod-1"), // duplicate, self
	}
	got := EnumeratePairs(items)
	if len(got) != 0 {
		t.Errorf("pair count=%d want 0 (identical series must not pair)", len(got))
	}
}

// TestEnumeratePairsSuspectToVictimOnly 는 #406 의 방향 사전필터를 검증한다. suspect (cause score)
// 에서 victim (latency) 으로 가는 페어만 생성되고, 역방향과 suspect↔suspect 와 victim↔victim 은
// SelectTopN 이 채택하지 않는 헛계산이라 enumerate 단계에서 제외된다.
func TestEnumeratePairsSuspectToVictimOnly(t *testing.T) {
	items := []LabeledSeries{
		ls("pod:cpu_throttle_score:5m", "node-a", "ns", "pod-1"),
		ls("pod:memory_pressure_score:5m", "node-a", "ns", "pod-2"),
		ls("pod:stage_latency_p99:5m", "node-a", "ns", "pod-3"),
		ls("pod:stage_latency_p99:5m", "node-a", "ns", "pod-4"),
	}
	got := pairKeys(EnumeratePairs(items))
	want := []string{
		"pod:cpu_throttle_score:5m@ns/pod-1 -> pod:stage_latency_p99:5m@ns/pod-3",
		"pod:cpu_throttle_score:5m@ns/pod-1 -> pod:stage_latency_p99:5m@ns/pod-4",
		"pod:memory_pressure_score:5m@ns/pod-2 -> pod:stage_latency_p99:5m@ns/pod-3",
		"pod:memory_pressure_score:5m@ns/pod-2 -> pod:stage_latency_p99:5m@ns/pod-4",
	}
	if !equalStringSlices(got, want) {
		t.Errorf("\n got=%v\nwant=%v (suspect→victim only)", got, want)
	}
}

// TestEnumeratePairsSamePodCrossMetric 는 같은 Pod 의 서로 다른 metric 페어 (cause score 와
// latency) 가 생성되는지 검증한다. 동일 Pod 의 self 정의는 (namespace, pod, metric) 셋 모두 같을
// 때만 self 로 보고 metric 이 다르면 정상 페어다.
func TestEnumeratePairsSamePodCrossMetric(t *testing.T) {
	items := []LabeledSeries{
		ls("pod:cpu_throttle_score:5m", "node-a", "ns", "pod-1"),
		ls("pod:stage_latency_p99:5m", "node-a", "ns", "pod-1"),
	}
	got := pairKeys(EnumeratePairs(items))
	want := []string{
		"pod:cpu_throttle_score:5m@ns/pod-1 -> pod:stage_latency_p99:5m@ns/pod-1",
	}
	if !equalStringSlices(got, want) {
		t.Errorf("\n got=%v\nwant=%v (same-pod cross-metric must pair)", got, want)
	}
}

// TestEnumeratePairsEmptyNodeLabelExcluded 는 node 라벨이 비어 있는 entry 가 pair 후보에서 제외
// 되는지 검증한다. node 정보 없는 시계열은 어떤 노드에 속하는지 결정 불가라 cross-node 가드를
// 적용할 수 없다.
func TestEnumeratePairsEmptyNodeLabelExcluded(t *testing.T) {
	items := []LabeledSeries{
		{Metric: "pod:stage_latency_p99:5m", Series: TimeSeries{Labels: map[string]string{"src_namespace": "ns", "src_pod": "pod-1"}}},
		ls("pod:cpu_throttle_score:5m", "node-a", "ns", "pod-2"),
	}
	got := EnumeratePairs(items)
	if len(got) != 0 {
		t.Errorf("pair count=%d want 0 (entries without node label must be excluded)", len(got))
	}
}

// TestEnumeratePairsNodeLevelMetricExcluded 는 node 라벨만 있고 namespace / pod 라벨이 없는
// node-level 메트릭 (예: node:gpu_idle:5m) 이 Pod 페어 후보에서 제외되는지 검증한다. PairKey 의
// schema 가 Pod 페어 기반이라 namespace / pod 가 빈 series 와의 페어는 schema 거짓말이 되어 제외
// 가 필요하다.
func TestEnumeratePairsNodeLevelMetricExcluded(t *testing.T) {
	items := []LabeledSeries{
		// node-level series: node 만 있고 namespace / pod 라벨 없음
		{Metric: "node:netobs_pod_stage_latency_p99:5m", Series: TimeSeries{Labels: map[string]string{"node": "node-a"}}},
		ls("pod:cpu_throttle_score:5m", "node-a", "ns", "pod-1"),
	}
	got := EnumeratePairs(items)
	if len(got) != 0 {
		t.Errorf("pair count=%d want 0 (node-level series must not pair with pod series)", len(got))
	}
}

// TestEnumeratePairsPreservesPodUID 는 src_pod_uid 라벨이 PairKey 의 SrcPodUID / DstPodUID 로
// 정확히 전달되는지 검증한다. #51 exporter 가 UID 라벨로 시계열을 식별해야 하므로 결과 schema 에
// UID 가 누락되면 안 된다.
func TestEnumeratePairsPreservesPodUID(t *testing.T) {
	items := []LabeledSeries{
		ls("pod:cpu_throttle_score:5m", "node-a", "ns", "pod-1"), // uid-ns-pod-1
		ls("pod:stage_latency_p99:5m", "node-a", "ns", "pod-2"),  // uid-ns-pod-2
	}
	pairs := EnumeratePairs(items)
	if len(pairs) != 1 {
		t.Fatalf("pair count=%d want 1", len(pairs))
	}
	for _, p := range pairs {
		if p.Key.SrcPodUID == "" || p.Key.DstPodUID == "" {
			t.Errorf("pair %+v missing UID", p.Key)
		}
		wantSrc := "uid-ns-" + p.Key.SrcPod
		wantDst := "uid-ns-" + p.Key.DstPod
		if p.Key.SrcPodUID != wantSrc || p.Key.DstPodUID != wantDst {
			t.Errorf("UIDs=%q/%q want %q/%q", p.Key.SrcPodUID, p.Key.DstPodUID, wantSrc, wantDst)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
