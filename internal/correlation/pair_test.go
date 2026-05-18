package correlation

import (
	"sort"
	"testing"
)

// ls 는 테스트용 LabeledSeries 짧은 생성자다. metric / node / namespace / pod 만 지정하고 시계열
// 데이터는 의미가 없으니 빈 슬라이스로 둔다.
func ls(metric, node, ns, pod string) LabeledSeries {
	return LabeledSeries{
		Metric: metric,
		Series: TimeSeries{
			Labels: map[string]string{
				"node":          node,
				"src_namespace": ns,
				"src_pod":       pod,
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
		ls("m1", "node-a", "ns", "pod-1"),
		ls("m1", "node-a", "ns", "pod-2"),
		ls("m1", "node-b", "ns", "pod-3"),
	}
	got := pairKeys(EnumeratePairs(items))
	want := []string{
		"m1@ns/pod-1 -> m1@ns/pod-2",
		"m1@ns/pod-2 -> m1@ns/pod-1",
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

// TestEnumeratePairsAsymmetricBothDirections 는 (X, Y) 와 (Y, X) 가 별도 페어로 둘 다 생성되는지
// 검증한다. 비대칭 분석 (src 자원 → dst latency vs dst 자원 → src latency) 의 기반.
func TestEnumeratePairsAsymmetricBothDirections(t *testing.T) {
	items := []LabeledSeries{
		ls("cpu", "node-a", "ns", "pod-1"),
		ls("latency", "node-a", "ns", "pod-2"),
	}
	got := pairKeys(EnumeratePairs(items))
	want := []string{
		"cpu@ns/pod-1 -> latency@ns/pod-2",
		"latency@ns/pod-2 -> cpu@ns/pod-1",
	}
	if !equalStringSlices(got, want) {
		t.Errorf("\n got=%v\nwant=%v (both directions must exist)", got, want)
	}
}

// TestEnumeratePairsSamePodCrossMetric 는 같은 Pod 의 서로 다른 metric 페어 (cause score 와
// latency) 가 생성되는지 검증한다. 동일 Pod 의 self 정의는 (namespace, pod, metric) 셋 모두 같을
// 때만 self 로 보고 metric 이 다르면 정상 페어다.
func TestEnumeratePairsSamePodCrossMetric(t *testing.T) {
	items := []LabeledSeries{
		ls("cpu_throttle_score", "node-a", "ns", "pod-1"),
		ls("latency_p99", "node-a", "ns", "pod-1"),
	}
	got := pairKeys(EnumeratePairs(items))
	want := []string{
		"cpu_throttle_score@ns/pod-1 -> latency_p99@ns/pod-1",
		"latency_p99@ns/pod-1 -> cpu_throttle_score@ns/pod-1",
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
		{Metric: "m1", Series: TimeSeries{Labels: map[string]string{"src_namespace": "ns", "src_pod": "pod-1"}}},
		ls("m1", "node-a", "ns", "pod-2"),
	}
	got := EnumeratePairs(items)
	if len(got) != 0 {
		t.Errorf("pair count=%d want 0 (entries without node label must be excluded)", len(got))
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
