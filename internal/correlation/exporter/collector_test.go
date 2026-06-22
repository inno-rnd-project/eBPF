package exporter

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/correlation"
)

func neighbor(victimPod, suspectPod string, dim correlation.ResourceDimension, rank int, score float64, lag int) correlation.NoisyNeighbor {
	return correlation.NoisyNeighbor{
		Victim:        correlation.PodIdentity{Namespace: "default", Pod: victimPod, PodUID: "uid-" + victimPod},
		VictimMetric:  "latency",
		VictimSignal:  correlation.SignalLatency,
		Suspect:       correlation.PodIdentity{Namespace: "default", Pod: suspectPod, PodUID: "uid-" + suspectPod},
		SuspectMetric: "pod:cpu_throttle_score:5m",
		Dimension:     dim,
		Rank:          rank,
		Score:         score,
		LagSteps:      lag,
		SampleCount:   120,
	}
}

// TestCollector_EmitsScoreAndLag 는 snapshot 의 각 NoisyNeighbor 가 score 와 lag 두 series 로
// emit 되며 lag 가 step 과 곱해져 초 단위로 환산되는지 검증한다.
func TestCollector_EmitsScoreAndLag(t *testing.T) {
	c := NewCollector(30 * time.Second)
	c.Replace([]correlation.NoisyNeighbor{
		neighbor("v1", "s1", correlation.DimensionCPU, 1, 0.85, 2),
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	expected := `
# HELP correlation_noisy_neighbor_lag_seconds score 가 최대 절대값을 보인 lag 의 초 단위 환산. 양수면 suspect 변동이 victim latency 를 N 초 선행하는 인과 방향이다.
# TYPE correlation_noisy_neighbor_lag_seconds gauge
correlation_noisy_neighbor_lag_seconds{rank="1",resource_dimension="cpu",suspect_namespace="default",suspect_pod="s1",suspect_pod_uid="uid-s1",victim_namespace="default",victim_pod="v1",victim_pod_uid="uid-v1",victim_signal="latency"} 60
# HELP correlation_noisy_neighbor_score Pearson 상관계수 최대 절대값. 1.0 에 가까울수록 suspect 자원 압박과 victim latency 가 강한 동조를 보인다.
# TYPE correlation_noisy_neighbor_score gauge
correlation_noisy_neighbor_score{rank="1",resource_dimension="cpu",suspect_namespace="default",suspect_pod="s1",suspect_pod_uid="uid-s1",victim_namespace="default",victim_pod="v1",victim_pod_uid="uid-v1",victim_signal="latency"} 0.85
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"correlation_noisy_neighbor_score", "correlation_noisy_neighbor_lag_seconds"); err != nil {
		t.Errorf("metric mismatch: %v", err)
	}
}

// TestCollector_SnapshotReturnsIndependentCopy 는 Snapshot 이 내부 상태와 분리된 안전한 복사본을
// 반환해 호출자가 결과를 수정해도 다음 Snapshot / Collect 가 영향받지 않는지 검증한다. rca-summarizer
// 가 본 결과를 mutate 할 일은 없지만 race 안전성과 캡슐화 정합성의 회귀 가드다.
func TestCollector_SnapshotReturnsIndependentCopy(t *testing.T) {
	c := NewCollector(30 * time.Second)
	c.Replace([]correlation.NoisyNeighbor{
		neighbor("v1", "s1", correlation.DimensionCPU, 1, 0.85, 2),
		neighbor("v1", "s2", correlation.DimensionGPU, 1, 0.70, 0),
	})

	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len=%d; want 2", len(snap))
	}

	// 호출자가 반환값을 mutate 해도 내부 상태가 보존되어야 한다.
	snap[0].Score = -1
	snap = snap[:0]

	again := c.Snapshot()
	if len(again) != 2 {
		t.Errorf("snapshot len=%d after caller truncation; want 2 (internal state must be isolated)", len(again))
	}
	if again[0].Score != 0.85 {
		t.Errorf("snapshot[0].Score=%v after caller mutation; want 0.85", again[0].Score)
	}
}

// TestCollector_SnapshotEmptyBeforeReplace 는 Replace 호출 전 Snapshot 이 nil 을 반환해 첫 reconcile
// 전 stale 값을 노출하지 않는지 검증한다.
func TestCollector_SnapshotEmptyBeforeReplace(t *testing.T) {
	c := NewCollector(30 * time.Second)
	if got := c.Snapshot(); got != nil {
		t.Errorf("Snapshot=%v before Replace; want nil", got)
	}
}

// TestCollector_ReplaceClearsStale 는 직전 snapshot 의 라벨이 다음 snapshot 에서 자동으로 사라지는지
// 검증한다 (stale series GC 회귀 가드).
func TestCollector_ReplaceClearsStale(t *testing.T) {
	c := NewCollector(30 * time.Second)
	c.Replace([]correlation.NoisyNeighbor{
		neighbor("v1", "s-stale", correlation.DimensionCPU, 1, 0.9, 0),
	})

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	if count := testutil.CollectAndCount(c, "correlation_noisy_neighbor_score"); count != 1 {
		t.Fatalf("initial count=%d want 1", count)
	}

	c.Replace([]correlation.NoisyNeighbor{
		neighbor("v1", "s-fresh", correlation.DimensionMemory, 1, 0.8, 0),
	})

	if count := testutil.CollectAndCount(c, "correlation_noisy_neighbor_score"); count != 1 {
		t.Fatalf("after replace count=%d want 1 (stale must be gone)", count)
	}

	// stale suspect 라벨이 사라졌는지 직접 검증한다. ToFloat64 는 단일 metric 한정이라 gather 로
	// 라벨 셋을 직접 확인한다.
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "correlation_noisy_neighbor_score" {
			continue
		}
		for _, m := range mf.Metric {
			for _, lp := range m.Label {
				if lp.GetName() == "suspect_pod" && lp.GetValue() == "s-stale" {
					t.Errorf("stale suspect label survived replace: %s=%s", lp.GetName(), lp.GetValue())
				}
			}
		}
	}
}

// TestCollector_EmptySnapshot 은 첫 reconcile 전 snapshot 이 nil 일 때 noisy_neighbor 메트릭이
// 0 series 로 emit 되는지 검증한다 (stale 0 값 emit 방지).
func TestCollector_EmptySnapshot(t *testing.T) {
	c := NewCollector(30 * time.Second)
	if count := testutil.CollectAndCount(c, "correlation_noisy_neighbor_score"); count != 0 {
		t.Errorf("nil snapshot count=%d want 0", count)
	}

	c.Replace(nil)
	if count := testutil.CollectAndCount(c, "correlation_noisy_neighbor_score"); count != 0 {
		t.Errorf("nil replace count=%d want 0", count)
	}

	c.Replace([]correlation.NoisyNeighbor{})
	if count := testutil.CollectAndCount(c, "correlation_noisy_neighbor_score"); count != 0 {
		t.Errorf("empty replace count=%d want 0", count)
	}
}

// TestCollector_EmitsVictimSignalLabel 은 #150 의 victim_signal 라벨이 latency / throughput 신호별로
// 구분 emit 되는지 검증한다. throughput victim 은 ImpactOK=false 라 impact_seconds 시리즈가 없어야 한다.
func TestCollector_EmitsVictimSignalLabel(t *testing.T) {
	c := NewCollector(30 * time.Second)
	tput := neighbor("v1", "s1", correlation.DimensionCPU, 1, 0.8, 1)
	tput.VictimSignal = correlation.SignalThroughput
	tput.Impact = 0.05
	tput.ImpactOK = false // SelectTopN 이 latency 외 victim 의 impact 를 gate
	c.Replace([]correlation.NoisyNeighbor{tput})

	want := `# HELP correlation_noisy_neighbor_score Pearson 상관계수 최대 절대값. 1.0 에 가까울수록 suspect 자원 압박과 victim latency 가 강한 동조를 보인다.
# TYPE correlation_noisy_neighbor_score gauge
correlation_noisy_neighbor_score{rank="1",resource_dimension="cpu",suspect_namespace="default",suspect_pod="s1",suspect_pod_uid="uid-s1",victim_namespace="default",victim_pod="v1",victim_pod_uid="uid-v1",victim_signal="throughput"} 0.8
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "correlation_noisy_neighbor_score"); err != nil {
		t.Errorf("throughput victim_signal emit mismatch:\n%v", err)
	}
	if count := testutil.CollectAndCount(c, "correlation_noisy_neighbor_impact_seconds"); count != 0 {
		t.Errorf("impact series=%d want 0 (throughput victim 은 impact gate)", count)
	}
}

// TestCollector_EmitsCrossNodeScore 는 ReplaceCrossNode 가 보관한 NodeInterference snapshot 이
// correlation_cross_node_score gauge 로 정확히 emit 되는지 검증한다. victim_node, suspect_node,
// dimension 3 라벨 셋이 라벨 셋 분리 정책에 정합하는지 회귀 가드 다.
func TestCollector_EmitsCrossNodeScore(t *testing.T) {
	c := NewCollector(30 * time.Second)
	c.ReplaceCrossNode([]correlation.NodeInterference{
		{
			VictimNode:  "gpu",
			SuspectNode: "ebpf-worker1",
			Dimension:   correlation.DimensionCPU,
			Rank:        1,
			Score:       0.82,
			LagSteps:    1,
			SampleCount: 60,
		},
	})
	want := `# HELP correlation_cross_node_score #84 cross-node interference layer 의 Pearson 상관계수 최대 절대값. suspect_node 의 자원 압박 (dimension) 과 victim_node 의 p99 latency 사이의 동조 정도다. CrossNodeEnabled opt-in 시 만 emit 되며 victim_node == suspect_node 인 시리즈는 enumerate 단에서 자동 제외된다.
# TYPE correlation_cross_node_score gauge
correlation_cross_node_score{dimension="cpu",suspect_node="ebpf-worker1",victim_node="gpu"} 0.82
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "correlation_cross_node_score"); err != nil {
		t.Errorf("cross_node_score emit mismatch:\n%v", err)
	}
}

// TestCollector_CrossNodeEmptySnapshot 은 ReplaceCrossNode 미호출 또는 빈 슬라이스 호출 시 series 가
// 0 개 emit 되어 CrossNodeEnabled=false 비활성 운영 모드 가 series 폭주 없이 유지 되는지 확인한다.
func TestCollector_CrossNodeEmptySnapshot(t *testing.T) {
	c := NewCollector(30 * time.Second)
	if count := testutil.CollectAndCount(c, "correlation_cross_node_score"); count != 0 {
		t.Errorf("nil cross_node count=%d want 0", count)
	}
	c.ReplaceCrossNode(nil)
	if count := testutil.CollectAndCount(c, "correlation_cross_node_score"); count != 0 {
		t.Errorf("nil replace count=%d want 0", count)
	}
	c.ReplaceCrossNode([]correlation.NodeInterference{})
	if count := testutil.CollectAndCount(c, "correlation_cross_node_score"); count != 0 {
		t.Errorf("empty replace count=%d want 0", count)
	}
}

// TestCollector_ConcurrentReplaceAndCollect 는 reconcile 의 Replace 와 Prometheus scrape 의 Collect
// 가 동시 호출되어도 race 가 발생하지 않는지 -race 빌드에서 검증한다.
func TestCollector_ConcurrentReplaceAndCollect(t *testing.T) {
	c := NewCollector(30 * time.Second)
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				c.Replace([]correlation.NoisyNeighbor{
					neighbor("v1", "s1", correlation.DimensionCPU, 1, float64(i%100)/100.0, i%3),
				})
				i++
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = reg.Gather()
		}
	}()

	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()
}

// TestHealth_RecordCycleAccumulates 는 RecordCycle 호출이 누적 카운터를 정확히 갱신하는지 검증한다.
func TestHealth_RecordCycleAccumulates(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHealth(reg)

	results := []correlation.CorrelationResult{
		{Status: correlation.StatusOK},
		{Status: correlation.StatusOK},
		{Status: correlation.StatusSkippedLowSamples},
		{Status: correlation.StatusSkippedConstant},
		{Status: correlation.StatusPartial},
	}
	neighbors := []correlation.NoisyNeighbor{
		neighbor("v1", "s1", correlation.DimensionCPU, 1, 0.7, 0),
		neighbor("v1", "s2", correlation.DimensionCPU, 2, 0.6, 0),
	}

	h.RecordCycle(150*time.Millisecond, results, neighbors, 7)

	if v := testutil.ToFloat64(h.ReconcilePairs); v != 5 {
		t.Errorf("pairs_total=%v want 5", v)
	}
	if v := testutil.ToFloat64(h.ReconcileNeighbors); v != 2 {
		t.Errorf("neighbors_total=%v want 2", v)
	}
	if v := testutil.ToFloat64(h.ReconcileSkipped.WithLabelValues("low_samples")); v != 1 {
		t.Errorf("skipped low_samples=%v want 1", v)
	}
	if v := testutil.ToFloat64(h.ReconcileSkipped.WithLabelValues("constant")); v != 1 {
		t.Errorf("skipped constant=%v want 1", v)
	}
	if v := testutil.ToFloat64(h.ReconcileDuration); v != 0.15 {
		t.Errorf("duration=%v want 0.15", v)
	}
	if v := testutil.ToFloat64(h.LastSuccessTimestamp); v == 0 {
		t.Errorf("last_success_timestamp=0 want >0 after RecordCycle")
	}

	h.RecordCycle(200*time.Millisecond, results, neighbors, 7)
	if v := testutil.ToFloat64(h.ReconcilePairs); v != 10 {
		t.Errorf("pairs_total after second cycle=%v want 10 (누적)", v)
	}
}

// TestCollector_PValueEmittedOnlyWhenGrangerOK 는 NoisyNeighbor.GrangerOK 가 true 인 페어만
// correlation_noisy_neighbor_pvalue 시리즈로 emit 되고 false 인 페어는 emit 자체가 skip 되는지
// 검증한다. continuous p-value 의 cardinality 가드 의도가 회귀에 영향받지 않게 한다.
func TestCollector_PValueEmittedOnlyWhenGrangerOK(t *testing.T) {
	c := NewCollector(30 * time.Second)
	ok := neighbor("v-ok", "s1", correlation.DimensionCPU, 1, 0.9, 0)
	ok.GrangerOK = true
	ok.PValue = 0.01
	notOK := neighbor("v-skip", "s2", correlation.DimensionMemory, 1, 0.7, 0)
	notOK.GrangerOK = false
	notOK.PValue = 0
	c.Replace([]correlation.NoisyNeighbor{ok, notOK})

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var pvalueCount int
	for _, mf := range mfs {
		if mf.GetName() == "correlation_noisy_neighbor_pvalue" {
			pvalueCount = len(mf.Metric)
		}
	}
	if pvalueCount != 1 {
		t.Errorf("pvalue series count=%d want 1 (GrangerOK=true 한 페어만 emit)", pvalueCount)
	}
}

// TestCollector_DominantDimensionEmitted 는 victim 단위 dominant dimension 산정 결과가
// correlation_dominant_dimension 시리즈로 emit 되며 4 dimension 합이 0 인 victim 은 자연 제외되는지
// 검증한다.
func TestCollector_DominantDimensionEmitted(t *testing.T) {
	c := NewCollector(30 * time.Second)
	// v1 은 cpu 0.9, memory 0.1 → dominant cpu.
	v1Cpu := neighbor("v1", "s1", correlation.DimensionCPU, 1, 0.9, 0)
	v1Mem := neighbor("v1", "s2", correlation.DimensionMemory, 1, 0.1, 0)
	// v-zero 는 모든 score 0 → 시리즈 미존재 기대.
	vZero := neighbor("v-zero", "s3", correlation.DimensionGPU, 1, 0, 0)
	c.Replace([]correlation.NoisyNeighbor{v1Cpu, v1Mem, vZero})

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var emitted []string
	for _, mf := range mfs {
		if mf.GetName() != "correlation_dominant_dimension" {
			continue
		}
		for _, m := range mf.Metric {
			var victim, dim string
			for _, lp := range m.Label {
				switch lp.GetName() {
				case "victim_pod":
					victim = lp.GetValue()
				case "dimension":
					dim = lp.GetValue()
				}
			}
			emitted = append(emitted, victim+"/"+dim)
		}
	}
	if len(emitted) != 1 {
		t.Fatalf("dominant_dimension series count=%d want 1 (v1 한정, v-zero 는 제외) got=%v", len(emitted), emitted)
	}
	if emitted[0] != "v1/cpu" {
		t.Errorf("got=%q want v1/cpu", emitted[0])
	}
}

// TestHealth_RecordErrorDoesNotTouchSuccessTimestamp 는 RecordError 가 LastSuccessTimestamp 를
// 갱신하지 않아 CorrelationExporterStalled alert 가 발화 가능한지 검증한다.
func TestHealth_RecordErrorDoesNotTouchSuccessTimestamp(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHealth(reg)

	h.RecordError()
	h.RecordError()

	if v := testutil.ToFloat64(h.ReconcileErrors); v != 2 {
		t.Errorf("errors_total=%v want 2", v)
	}
	if v := testutil.ToFloat64(h.LastSuccessTimestamp); v != 0 {
		t.Errorf("last_success_timestamp=%v want 0 (error 가 timestamp 를 갱신해서는 안 됨)", v)
	}
}

// TestCollector_EmitsImpact 는 #146 의 effect size 가 ImpactOK=true 일 때 correlation_noisy_neighbor_
// impact_seconds series 로 emit 되고, ImpactOK=false 일 때는 emit 되지 않아 0 noise 가 끼지 않는지
// 검증한다.
func TestCollector_EmitsImpact(t *testing.T) {
	c := NewCollector(30 * time.Second)

	withImpact := neighbor("v1", "s1", correlation.DimensionCPU, 1, 0.85, 2)
	withImpact.Impact = 0.042
	withImpact.ImpactOK = true
	noImpact := neighbor("v2", "s2", correlation.DimensionCPU, 1, 0.80, 1)
	noImpact.ImpactOK = false
	c.Replace([]correlation.NoisyNeighbor{withImpact, noImpact})

	// ImpactOK=true 인 1 개만 emit 되어야 한다.
	if count := testutil.CollectAndCount(c, "correlation_noisy_neighbor_impact_seconds"); count != 1 {
		t.Fatalf("impact series=%d want 1 (ImpactOK=true 만 emit)", count)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	expected := `
# HELP correlation_noisy_neighbor_impact_seconds #146 의 effect size. suspect 압박 구간과 비압박 구간의 victim latency 차이 (seconds) 로 간섭의 절대 영향 크기다. score (상관 강도) 가 동조 여부를 본다면 본 메트릭은 victim 을 실제로 얼마나 느리게 만들었는지의 크기를 노출해 운영자가 우선순위를 판단하게 한다. 표본 부족 등으로 산정이 skip (ImpactOK=false) 된 시리즈는 emit 되지 않아 0 noise 가 끼지 않는다.
# TYPE correlation_noisy_neighbor_impact_seconds gauge
correlation_noisy_neighbor_impact_seconds{rank="1",resource_dimension="cpu",suspect_namespace="default",suspect_pod="s1",suspect_pod_uid="uid-s1",victim_namespace="default",victim_pod="v1",victim_pod_uid="uid-v1",victim_signal="latency"} 0.042
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "correlation_noisy_neighbor_impact_seconds"); err != nil {
		t.Errorf("impact metric mismatch: %v", err)
	}
}

// TestCollector_EmitsServiceImpactScore 는 ReplaceServiceImpact 가 보관한 ServiceImpact snapshot 이
// correlation_service_impact_score gauge 로 정확히 emit 되는지 검증한다. victim_namespace,
// victim_workload, suspect_node, dimension 4 라벨 셋이 라벨 셋 분리 정책에 정합하는지 회귀 가드다.
func TestCollector_EmitsServiceImpactScore(t *testing.T) {
	c := NewCollector(30 * time.Second)
	c.ReplaceServiceImpact([]correlation.ServiceImpact{
		{
			VictimNamespace: "default",
			VictimWorkload:  "api",
			SuspectNode:     "ebpf-worker1",
			Dimension:       correlation.DimensionCPU,
			Rank:            1,
			Score:           0.82,
			LagSteps:        1,
			SampleCount:     60,
		},
	})
	want := `# HELP correlation_service_impact_score #148 service-impact layer 의 Pearson 상관계수 최대 절대값. suspect_node 의 자원 압박 (dimension) 과 victim workload (K8s Service 근사, namespace/workload) 의 p99 latency 사이의 동조 정도다. victim 은 netobs 의 src_workload 라벨로 집계되며 ServiceImpactEnabled opt-in 시 만 emit 된다.
# TYPE correlation_service_impact_score gauge
correlation_service_impact_score{dimension="cpu",suspect_node="ebpf-worker1",victim_namespace="default",victim_workload="api"} 0.82
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "correlation_service_impact_score"); err != nil {
		t.Errorf("service_impact_score emit mismatch:\n%v", err)
	}
}

// TestCollector_ServiceImpactEmptySnapshot 은 ReplaceServiceImpact 미호출 또는 빈 슬라이스 호출 시
// series 가 0 개 emit 되어 ServiceImpactEnabled=false opt-out 운영 모드가 series 폭주 없이 유지되는지
// 확인한다.
func TestCollector_ServiceImpactEmptySnapshot(t *testing.T) {
	c := NewCollector(30 * time.Second)
	if count := testutil.CollectAndCount(c, "correlation_service_impact_score"); count != 0 {
		t.Errorf("nil service_impact count=%d want 0", count)
	}
	c.ReplaceServiceImpact(nil)
	if count := testutil.CollectAndCount(c, "correlation_service_impact_score"); count != 0 {
		t.Errorf("nil replace count=%d want 0", count)
	}
	c.ReplaceServiceImpact([]correlation.ServiceImpact{})
	if count := testutil.CollectAndCount(c, "correlation_service_impact_score"); count != 0 {
		t.Errorf("empty replace count=%d want 0", count)
	}
}

// TestHealth_RecordCycleServiceImpactObserved 는 IsServiceImpact 결과의 ServiceImpactPair metric 이
// observed distinct metric 집계에 정확히 반영되어 ReconcilePartial 이 거짓 증가하지 않는지 검증한다.
// 분기가 빠지면 빈 r.Pair metric ("") 이 끼어 observed 가 왜곡된다.
func TestHealth_RecordCycleServiceImpactObserved(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHealth(reg)

	// suspect node 압박 2종 + victim workload latency 1종 = distinct 3종.
	results := []correlation.CorrelationResult{
		{
			Status:          correlation.StatusOK,
			IsServiceImpact: true,
			ServiceImpactPair: correlation.ServiceImpactPairKey{
				SuspectMetric: "node:cpu_pressure_score:5m",
				VictimMetric:  "workload:netobs_stage_latency_p99:5m",
			},
		},
		{
			Status:          correlation.StatusOK,
			IsServiceImpact: true,
			ServiceImpactPair: correlation.ServiceImpactPairKey{
				SuspectMetric: "node:memory_pressure_score:5m",
				VictimMetric:  "workload:netobs_stage_latency_p99:5m",
			},
		},
	}
	// expected=3 (distinct) 이면 observed=3 과 일치해 partial 이 증가하지 않아야 한다.
	h.RecordCycle(10*time.Millisecond, results, nil, 3)
	if v := testutil.ToFloat64(h.ReconcileMetricsObserved); v != 3 {
		t.Errorf("observed=%v want 3 (service-impact 분기 미반영)", v)
	}
	if v := testutil.ToFloat64(h.ReconcilePartial); v != 0 {
		t.Errorf("partial=%v want 0 (observed==expected 인데 거짓 증가)", v)
	}
}

// TestCollector_EmitsCrossLevelScore 는 ReplaceCrossLevel 가 보관한 CrossLevel snapshot 이
// correlation_cross_level_score gauge 로 정확히 emit 되는지 검증한다. node, pod_namespace, pod,
// direction, dimension 5 라벨 셋이 라벨 셋 분리 정책에 정합하는지 회귀 가드다.
func TestCollector_EmitsCrossLevelScore(t *testing.T) {
	c := NewCollector(30 * time.Second)
	c.ReplaceCrossLevel([]correlation.CrossLevel{
		{
			Node:         "ebpf-worker1",
			Direction:    correlation.DirectionNodeToPod,
			PodNamespace: "default",
			Pod:          "api-0",
			Dimension:    correlation.DimensionCPU,
			Rank:         1,
			Score:        0.82,
			LagSteps:     1,
			SampleCount:  60,
		},
	})
	want := `# HELP correlation_cross_level_score #149 cross-level layer 의 Pearson 상관계수 최대 절대값. 동일 node 안에서 node 압박과 pod latency 사이의 동조 정도다. direction=node_to_pod 면 node 압박이 pod latency 에 주는 영향, pod_to_node 면 pod 압박이 node latency 에 주는 영향이며 dimension 은 압박 쪽에서 분류된다. CrossLevelEnabled opt-in 시 만 emit 된다.
# TYPE correlation_cross_level_score gauge
correlation_cross_level_score{dimension="cpu",direction="node_to_pod",node="ebpf-worker1",pod="api-0",pod_namespace="default"} 0.82
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want), "correlation_cross_level_score"); err != nil {
		t.Errorf("cross_level_score emit mismatch:\n%v", err)
	}
}

// TestCollector_CrossLevelEmptySnapshot 은 ReplaceCrossLevel 미호출 또는 빈 슬라이스 호출 시 series 가
// 0 개 emit 되어 CrossLevelEnabled=false opt-out 운영 모드가 series 폭주 없이 유지되는지 확인한다.
func TestCollector_CrossLevelEmptySnapshot(t *testing.T) {
	c := NewCollector(30 * time.Second)
	if count := testutil.CollectAndCount(c, "correlation_cross_level_score"); count != 0 {
		t.Errorf("nil cross_level count=%d want 0", count)
	}
	c.ReplaceCrossLevel(nil)
	if count := testutil.CollectAndCount(c, "correlation_cross_level_score"); count != 0 {
		t.Errorf("nil replace count=%d want 0", count)
	}
	c.ReplaceCrossLevel([]correlation.CrossLevel{})
	if count := testutil.CollectAndCount(c, "correlation_cross_level_score"); count != 0 {
		t.Errorf("empty replace count=%d want 0", count)
	}
}

// TestHealth_RecordCycleCrossLevelObserved 는 IsCrossLevel 결과의 CrossLevelPair metric 이 observed
// distinct metric 집계에 정확히 반영되어 ReconcilePartial 이 거짓 증가하지 않는지 검증한다. 분기가
// 빠지면 빈 r.Pair metric ("") 이 끼어 observed 가 왜곡된다.
func TestHealth_RecordCycleCrossLevelObserved(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHealth(reg)

	// node 압박 + pod latency + pod 압박 + node latency = distinct 4종.
	results := []correlation.CorrelationResult{
		{
			Status:       correlation.StatusOK,
			IsCrossLevel: true,
			CrossLevelPair: correlation.CrossLevelPairKey{
				SrcMetric: "node:cpu_pressure_score:5m",
				DstMetric: "pod_latency_q99",
			},
		},
		{
			Status:       correlation.StatusOK,
			IsCrossLevel: true,
			CrossLevelPair: correlation.CrossLevelPairKey{
				SrcMetric: "pod:cpu_throttle_score:5m",
				DstMetric: "node:netobs_pod_stage_latency_p99:5m",
			},
		},
	}
	h.RecordCycle(10*time.Millisecond, results, nil, 4)
	if v := testutil.ToFloat64(h.ReconcileMetricsObserved); v != 4 {
		t.Errorf("observed=%v want 4 (cross-level 분기 미반영)", v)
	}
	if v := testutil.ToFloat64(h.ReconcilePartial); v != 0 {
		t.Errorf("partial=%v want 0 (observed==expected 인데 거짓 증가)", v)
	}
}
