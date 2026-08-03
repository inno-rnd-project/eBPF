package correlation

import (
	"reflect"
	"testing"
)

// makeResult 는 테스트용 CorrelationResult 를 한 줄로 만든다. lag 와 sample count 는 본 패키지의
// 다른 곳에서 검증되므로 SelectTopN 의 책임만 격리해 본다.
func makeResult(srcNS, srcPod, srcUID, srcMetric, dstNS, dstPod, dstUID, dstMetric string,
	score float64, lag int, status Status,
) CorrelationResult {
	return CorrelationResult{
		Pair: PairKey{
			SrcNamespace: srcNS,
			SrcPod:       srcPod,
			SrcPodUID:    srcUID,
			SrcMetric:    srcMetric,
			DstNamespace: dstNS,
			DstPod:       dstPod,
			DstPodUID:    dstUID,
			DstMetric:    dstMetric,
		},
		MaxAbsValue: score,
		MaxAbsLag:   lag,
		SampleCount: 120,
		Status:      status,
	}
}

const latencyMetric = `histogram_quantile(0.99, sum by(node, src_namespace, src_pod, src_pod_uid, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))`

// TestSelectTopN_FiltersByLatencyVictim 은 noisy neighbor 모델의 페어 조건을 정확히 적용해 latency
// 메트릭이 페어 한쪽인 경우만 결과로 emit 하는지 검증한다.
func TestSelectTopN_FiltersByLatencyVictim(t *testing.T) {
	results := []CorrelationResult{
		// 양쪽 모두 non-latency: 제외.
		makeResult("default", "p1", "uid1", "pod:cpu_throttle_score:5m",
			"default", "p2", "uid2", "pod:memory_pressure_score:5m",
			0.9, 0, StatusOK),
		// 양쪽 모두 latency: 제외.
		makeResult("default", "p1", "uid1", latencyMetric,
			"default", "p2", "uid2", latencyMetric,
			0.8, 0, StatusOK),
		// Src=cpu suspect, Dst=latency victim: 채택.
		makeResult("default", "noisy", "uidN", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric,
			0.7, 1, StatusOK),
		// 반대 방향 (Src=latency, Dst=cpu): dedup 의미로 제외.
		makeResult("default", "victim", "uidV", latencyMetric,
			"default", "noisy", "uidN", "pod:cpu_throttle_score:5m",
			0.7, -1, StatusOK),
	}

	got := SelectTopN(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1, got=%+v", len(got), got)
	}
	if got[0].Victim.Pod != "victim" || got[0].Suspect.Pod != "noisy" {
		t.Errorf("victim/suspect=%s/%s want victim/noisy", got[0].Victim.Pod, got[0].Suspect.Pod)
	}
	if got[0].Dimension != DimensionCPU {
		t.Errorf("dimension=%s want cpu", got[0].Dimension)
	}
	if got[0].Rank != 1 {
		t.Errorf("rank=%d want 1", got[0].Rank)
	}
}

// TestSelectTopN_StatusFilter 는 OK / Partial 만 채택하고 SkippedConstant / SkippedLowSamples 는
// 결과에서 제외되는지 검증한다.
func TestSelectTopN_StatusFilter(t *testing.T) {
	cases := []struct {
		status Status
		want   bool
	}{
		{StatusOK, true},
		{StatusPartial, true},
		{StatusSkippedConstant, false},
		{StatusSkippedLowSamples, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			results := []CorrelationResult{
				makeResult("default", "noisy", "uidN", "pod:cpu_throttle_score:5m",
					"default", "victim", "uidV", latencyMetric,
					0.5, 0, tc.status),
			}
			got := SelectTopN(results, 10)
			if (len(got) == 1) != tc.want {
				t.Errorf("len=%d want=%v for status %s", len(got), tc.want, tc.status)
			}
		})
	}
}

// TestSelectTopN_DimensionClassification 은 query 문자열에서 dimension 이 정확히 분류되고
// 우선순위 (gpu > memory) 가 지켜지는지 검증한다.
func TestSelectTopN_DimensionClassification(t *testing.T) {
	cases := []struct {
		metric string
		want   ResourceDimension
	}{
		{"pod:cpu_throttle_score:5m", DimensionCPU},
		{"pod:memory_pressure_score:5m", DimensionMemory},
		{"pod:network_throughput_score:5m", DimensionNetwork},
		{"pod:network_retrans_score:5m", DimensionNetwork},
		{"pod:host_compute_stall_score:5m", DimensionCPU},
		{`avg by(node, src_namespace, src_pod, src_pod_uid) (pod:gpu_memory_utilization_ratio:5m)`, DimensionGPU},
		{"some_unknown_score", DimensionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			got := classifyDimension(tc.metric)
			if got != tc.want {
				t.Errorf("classifyDimension(%q)=%s want %s", tc.metric, got, tc.want)
			}
		})
	}
}

// TestSelectTopN_TopNBoundary 는 그룹당 후보 수가 topN 보다 많을 때 truncate, 적을 때 그대로
// 반환되는지 검증한다.
func TestSelectTopN_TopNBoundary(t *testing.T) {
	results := []CorrelationResult{}
	for i := 0; i < 5; i++ {
		results = append(results, makeResult(
			"default", "suspect-"+string(rune('a'+i)), "uid-"+string(rune('a'+i)), "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric,
			float64(5-i)*0.1, 0, StatusOK,
		))
	}

	got := SelectTopN(results, 3)
	if len(got) != 3 {
		t.Fatalf("topN=3 len=%d want 3", len(got))
	}
	for i, n := range got {
		if n.Rank != i+1 {
			t.Errorf("rank[%d]=%d want %d", i, n.Rank, i+1)
		}
	}
	if got[0].Score < got[1].Score || got[1].Score < got[2].Score {
		t.Errorf("scores not descending: %v %v %v", got[0].Score, got[1].Score, got[2].Score)
	}

	gotAll := SelectTopN(results, 10)
	if len(gotAll) != 5 {
		t.Errorf("topN=10 len=%d want 5 (전체 채택)", len(gotAll))
	}

	if SelectTopN(results, 0) != nil {
		t.Errorf("topN=0 want nil result")
	}
	if SelectTopN(results, -1) != nil {
		t.Errorf("topN=-1 want nil result")
	}
}

// TestSelectTopN_SamePodSelfPairExcluded 는 victim 의 cross-metric self pair (victim 의 cpu_throttle
// vs victim 의 latency) 가 suspect 후보에서 제외되는지 검증한다. EnumeratePairs 가 동일 Pod 의 두
// 다른 metric series 도 페어로 만들어 self-suspect 가 rank 1 을 차지할 위험을 차단하는 가드.
func TestSelectTopN_SamePodSelfPairExcluded(t *testing.T) {
	results := []CorrelationResult{
		// self-pair: victim 의 cpu_throttle 과 victim 의 latency. PodUID 동일.
		makeResult("default", "victim", "uidV", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric, 0.95, 0, StatusOK),
		// 정상 페어: 다른 Pod 의 cpu_throttle.
		makeResult("default", "noisy", "uidN", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric, 0.6, 0, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (self-pair 가 제외되어야 함)", len(got))
	}
	if got[0].Suspect.Pod == "victim" {
		t.Errorf("suspect=victim 이면 self-pair 가 통과한 것")
	}
}

// TestSelectTopN_SamePodSelfPairExcludedByNamespaceFallback 는 PodUID 가 둘 다 빈 문자열일 때
// namespace + pod 라벨로 same-pod 비교가 동작하는지 검증한다.
func TestSelectTopN_SamePodSelfPairExcludedByNamespaceFallback(t *testing.T) {
	results := []CorrelationResult{
		makeResult("default", "victim", "", "pod:cpu_throttle_score:5m",
			"default", "victim", "", latencyMetric, 0.95, 0, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 0 {
		t.Errorf("len=%d want 0 (UID 없는 self-pair 가 제외되어야 함)", len(got))
	}
}

// TestSelectTopN_DedupSameSuspectSameDimension 은 한 suspect 가 같은 dimension 의 두 다른 metric
// (cpu_throttle + host_compute_stall 둘 다 cpu) 으로 두 rank 를 차지하지 않고 max score 하나로
// 합쳐지는지 검증한다.
func TestSelectTopN_DedupSameSuspectSameDimension(t *testing.T) {
	results := []CorrelationResult{
		makeResult("default", "noisy", "uidN", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric, 0.6, 0, StatusOK),
		makeResult("default", "noisy", "uidN", "pod:host_compute_stall_score:5m",
			"default", "victim", "uidV", latencyMetric, 0.8, 0, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (같은 suspect / dimension 은 한 rank 만 차지)", len(got))
	}
	if got[0].Score != 0.8 {
		t.Errorf("score=%v want 0.8 (max 가 채택되어야 함)", got[0].Score)
	}
}

// TestSelectTopN_TieBreaker 는 동일 score 시 suspect 라벨 lexicographic 순서로 rank 가 결정되는지
// 검증한다.
func TestSelectTopN_TieBreaker(t *testing.T) {
	results := []CorrelationResult{
		makeResult("default", "b-suspect", "uidB", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric,
			0.5, 0, StatusOK),
		makeResult("default", "a-suspect", "uidA", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric,
			0.5, 0, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].Suspect.Pod != "a-suspect" || got[1].Suspect.Pod != "b-suspect" {
		t.Errorf("tie breaker order=%s,%s want a-suspect,b-suspect",
			got[0].Suspect.Pod, got[1].Suspect.Pod)
	}
}

// TestSelectTopN_GroupsByVictimAndDimension 은 다른 victim 또는 다른 dimension 이 별도 그룹으로
// 분리되어 각 그룹마다 rank 1 부터 시작하는지 검증한다.
func TestSelectTopN_GroupsByVictimAndDimension(t *testing.T) {
	results := []CorrelationResult{
		// victim A, cpu dimension.
		makeResult("default", "s1", "uid1", "pod:cpu_throttle_score:5m",
			"default", "vA", "uidA", latencyMetric, 0.9, 0, StatusOK),
		// victim A, memory dimension.
		makeResult("default", "s2", "uid2", "pod:memory_pressure_score:5m",
			"default", "vA", "uidA", latencyMetric, 0.8, 0, StatusOK),
		// victim B, cpu dimension.
		makeResult("default", "s3", "uid3", "pod:cpu_throttle_score:5m",
			"default", "vB", "uidB", latencyMetric, 0.7, 0, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	for _, n := range got {
		if n.Rank != 1 {
			t.Errorf("rank=%d want 1 for victim=%s dim=%s", n.Rank, n.Victim.Pod, n.Dimension)
		}
	}
}

// TestSelectTopN_UnknownDimensionSkipped 는 dimension 분류 실패 시 결과에서 제외되는지 검증한다.
// 카디널리티 가드의 회귀 방지.
func TestSelectTopN_UnknownDimensionSkipped(t *testing.T) {
	results := []CorrelationResult{
		makeResult("default", "weird", "uidW", "some_unrelated_metric",
			"default", "victim", "uidV", latencyMetric, 0.9, 0, StatusOK),
	}
	got := SelectTopN(results, 10)
	if len(got) != 0 {
		t.Errorf("len=%d want 0 (unknown dim must be filtered)", len(got))
	}
}

// TestSelectTopN_DeterministicOrder 는 동일 입력에 대해 결과 순서가 결정적인지 (map 순회
// 비결정성 차단 가드) 검증한다.
func TestSelectTopN_DeterministicOrder(t *testing.T) {
	results := []CorrelationResult{
		makeResult("ns-a", "s1", "u1", "pod:cpu_throttle_score:5m",
			"ns-a", "v1", "uv1", latencyMetric, 0.5, 0, StatusOK),
		makeResult("ns-b", "s2", "u2", "pod:memory_pressure_score:5m",
			"ns-b", "v2", "uv2", latencyMetric, 0.6, 0, StatusOK),
		makeResult("ns-a", "s3", "u3", "pod:network_throughput_score:5m",
			"ns-a", "v1", "uv1", latencyMetric, 0.4, 0, StatusOK),
	}
	first := SelectTopN(results, 10)
	for i := 0; i < 20; i++ {
		next := SelectTopN(results, 10)
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("non-deterministic order between runs:\n%+v\n%+v", first, next)
		}
	}
}

// TestSelectTopN_EmptyInput 은 빈 입력에서 panic 없이 빈 결과를 반환하는지 검증한다.
func TestSelectTopN_EmptyInput(t *testing.T) {
	got := SelectTopN(nil, 10)
	if len(got) != 0 {
		t.Errorf("nil input len=%d want 0", len(got))
	}
	got = SelectTopN([]CorrelationResult{}, 10)
	if len(got) != 0 {
		t.Errorf("empty input len=%d want 0", len(got))
	}
}

// TestSelectTopN_ImpactPropagation 은 #146 의 effect size 가 CorrelationResult 에서 NoisyNeighbor
// 로 전파되는지 검증한다. Impact 와 ImpactOK 는 Score 와 독립이라 채택된 candidate 의 값을 그대로
// 따라가야 한다.
func TestSelectTopN_ImpactPropagation(t *testing.T) {
	r := makeResult("ns", "suspect", "uid-s", "pod:cpu_throttle_score:5m",
		"ns", "victim", "uid-v", latencyMetric, 0.8, 1, StatusOK)
	r.Impact = 0.042
	r.ImpactOK = true

	got := SelectTopN([]CorrelationResult{r}, 10)
	if len(got) != 1 {
		t.Fatalf("got %d neighbors want 1", len(got))
	}
	if !got[0].ImpactOK {
		t.Errorf("ImpactOK=false want true")
	}
	if got[0].Impact != 0.042 {
		t.Errorf("Impact=%v want 0.042", got[0].Impact)
	}
}

// withSignedCorr 는 채택 lag 의 부호 있는 상관을 MaxAbsSignedValue 에 심는다 (#367 방향 게이트
// 검증용). makeResult 기본형은 MaxAbsSignedValue 가 0 이라 부호 미상 (중립 통과) 케이스를 겸한다.
func withSignedCorr(r CorrelationResult, signed float64) CorrelationResult {
	r.MaxAbsSignedValue = signed
	return r
}

// TestSelectTopN_DirectionGate_LatencyNegativeExcluded 는 latency victim 에서 역방향 (음) 강상관이
// rank 상위로 승격되지 않는지 검증한다 (#367). 종전 부호 무관 max|corr| 이면 -0.9 페어가 rank 1
// 이었다. 정방향 (+0.6) 페어만 채택되어 rank 1 이 된다.
func TestSelectTopN_DirectionGate_LatencyNegativeExcluded(t *testing.T) {
	results := []CorrelationResult{
		// 역방향 강상관: suspect 압박 증가에 latency 감소 (비간섭). 제외돼야 한다.
		withSignedCorr(makeResult("default", "inverse", "uidI", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", latencyMetric,
			0.9, 1, StatusOK), -0.9),
		// 정방향 상관: 간섭 후보. 채택돼야 한다.
		withSignedCorr(makeResult("default", "noisy", "uidN", "pod:memory_pressure_score:5m",
			"default", "victim", "uidV", latencyMetric,
			0.6, 1, StatusOK), 0.6),
	}

	got := SelectTopN(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (역방향 제외), got=%+v", len(got), got)
	}
	if got[0].Suspect.Pod != "noisy" || got[0].Rank != 1 || got[0].Score != 0.6 {
		t.Errorf("suspect=%s rank=%d score=%v want noisy/1/0.6", got[0].Suspect.Pod, got[0].Rank, got[0].Score)
	}
}

// TestSelectTopN_DirectionGate_GPUNegativeKept 는 gpu victim 의 음의 상관 의도 (#174) 가 유지되는지
// 검증한다 (#367). GPU 는 악화가 사용률 감소라 음의 상관이 간섭이고, 양의 상관 (압박 증가에 사용률
// 증가) 이 비간섭이라 제외된다.
func TestSelectTopN_DirectionGate_GPUNegativeKept(t *testing.T) {
	results := []CorrelationResult{
		// 음의 상관: GPU starvation 간섭. 채택돼야 한다.
		withSignedCorr(makeResult("default", "noisy", "uidN", "pod:cpu_throttle_score:5m",
			"default", "victim", "uidV", "pod:gpu_util_p95:5m",
			0.8, 0, StatusOK), -0.8),
		// 양의 상관: 비간섭. 제외돼야 한다.
		withSignedCorr(makeResult("default", "inverse", "uidI", "pod:memory_pressure_score:5m",
			"default", "victim", "uidV", "pod:gpu_util_p95:5m",
			0.9, 0, StatusOK), 0.9),
	}

	got := SelectTopN(results, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d want 1 (양의 상관 제외), got=%+v", len(got), got)
	}
	if got[0].Suspect.Pod != "noisy" || got[0].Score != 0.8 || got[0].VictimSignal != SignalGPU {
		t.Errorf("suspect=%s score=%v signal=%s want noisy/0.8/gpu", got[0].Suspect.Pod, got[0].Score, got[0].VictimSignal)
	}
}
