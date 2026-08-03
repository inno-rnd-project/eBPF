package correlation

import (
	"context"
	"math"
	"testing"
	"time"
)

// TestReconcilePipeline_DefaultOutputPinned 는 #406 메모리 구조 개선의 산출 불변 계약을 고정하는
// 회귀 테스트다. suspect (cpu cause score) 와 victim (stage latency) 시계열로 Correlate → SelectTopN
// 전체 경로를 흘려 최종 NoisyNeighbor 산출을 필드 단위로 단정한다. 파이프라인 내부 구조 (페어
// enumerate 방향, 캡, 사전 할당, lag map 표현) 가 바뀌어도 본 테스트가 green 이면 기본 구성의
// emit 산출이 동일함이 보장된다.
func TestReconcilePipeline_DefaultOutputPinned(t *testing.T) {
	labels := func(pod string) map[string]string {
		return map[string]string{"node": "n1", "src_namespace": "ns", "src_pod": pod, "src_pod_uid": "uid-" + pod}
	}
	// suspect: 두 pod 의 cpu cause score. p1 은 가파른 상승, p2 는 완만한 상승. victim: 두 pod 의
	// stage latency. p1 latency 는 p2 suspect 와, p2 latency 는 p1 suspect 와 각각 완전 선형 동조라
	// lag 0 에서 +1.0 상관이 잡힌다.
	s1 := linearSeries(labels("p1"), 60, 0, 1)
	s1.Metric = "pod:cpu_throttle_score:5m"
	s2 := linearSeries(labels("p2"), 60, 5, 0.5)
	s2.Metric = "pod:cpu_throttle_score:5m"
	v1 := linearSeries(labels("p1"), 60, 0.1, 0.002)
	v1.Metric = "pod:stage_latency_p99:5m"
	v2 := linearSeries(labels("p2"), 60, 0.2, 0.004)
	v2.Metric = "pod:stage_latency_p99:5m"

	fetcher := &mockFetcher{
		responses: map[string][]LabeledSeries{
			"pod:cpu_throttle_score:5m": {s1, s2},
			"pod:stage_latency_p99:5m":  {v1, v2},
		},
	}
	cfg := Config{
		Window:         60 * time.Second,
		Step:           1 * time.Second,
		MinSamples:     5,
		LagSteps:       []int{0},
		DefaultMetrics: []string{"pod:cpu_throttle_score:5m", "pod:stage_latency_p99:5m"},
	}
	results, err := New(fetcher, cfg).Correlate(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Correlate: %v", err)
	}

	neighbors := SelectTopN(results, 3)
	if len(neighbors) != 2 {
		t.Fatalf("neighbors=%d want 2 (victim latency 2개 x cpu suspect 1개씩)", len(neighbors))
	}
	// 비활성 layer 는 산출이 없어야 한다.
	if n := len(SelectTopNCrossNode(results, 3)); n != 0 {
		t.Errorf("cross_node=%d want 0", n)
	}
	if n := len(SelectTopNServiceImpact(results, 3)); n != 0 {
		t.Errorf("service_impact=%d want 0", n)
	}
	if n := len(SelectTopNCrossLevel(results, 3)); n != 0 {
		t.Errorf("cross_level=%d want 0", n)
	}

	// 정렬 규약 (victim namespace/pod 사전순) 에 따라 p1 victim 이 먼저다. 각 victim 의 suspect 는
	// 자기 자신 제외 규칙에 따라 상대 pod 다.
	type pinned struct {
		victimPod, suspectPod string
	}
	want := []pinned{{"p1", "p2"}, {"p2", "p1"}}
	for i, n := range neighbors {
		if n.Victim.Pod != want[i].victimPod || n.Suspect.Pod != want[i].suspectPod {
			t.Errorf("neighbors[%d]=(victim=%s suspect=%s) want (victim=%s suspect=%s)", i, n.Victim.Pod, n.Suspect.Pod, want[i].victimPod, want[i].suspectPod)
		}
		if n.Rank != 1 {
			t.Errorf("neighbors[%d].Rank=%d want 1", i, n.Rank)
		}
		if n.Dimension != DimensionCPU {
			t.Errorf("neighbors[%d].Dimension=%q want cpu", i, n.Dimension)
		}
		if n.VictimSignal != SignalLatency {
			t.Errorf("neighbors[%d].VictimSignal=%q want latency", i, n.VictimSignal)
		}
		if math.Abs(n.Score-1.0) > 1e-9 {
			t.Errorf("neighbors[%d].Score=%v want 1.0", i, n.Score)
		}
		if n.LagSteps != 0 {
			t.Errorf("neighbors[%d].LagSteps=%d want 0", i, n.LagSteps)
		}
		if n.SampleCount != 60 {
			t.Errorf("neighbors[%d].SampleCount=%d want 60", i, n.SampleCount)
		}
		// lag 0 은 Granger 검정 불가 (lag < 1) 라 GrangerOK=false 고, causal_strength 는 Pearson 항
		// 0.5 에 유의한 effect 항 0.2 가 더해진 0.7 로 고정된다 (Granger 항 0.3 은 0).
		if n.GrangerOK {
			t.Errorf("neighbors[%d].GrangerOK=true want false (lag 0)", i)
		}
		if math.Abs(n.CausalStrength-0.7) > 1e-6 {
			t.Errorf("neighbors[%d].CausalStrength=%v want 0.7", i, n.CausalStrength)
		}
		// effect size 는 latency victim 에서 산출되어야 한다 (값 자체는 EffectSize 단위 테스트가
		// 담당하고, 본 pin 은 산출 여부와 양의 방향만 고정한다).
		if !n.ImpactMagnitudeOK || n.ImpactMagnitude <= 0 {
			t.Errorf("neighbors[%d].ImpactMagnitude=%v (ok=%v) want >0", i, n.ImpactMagnitude, n.ImpactMagnitudeOK)
		}
	}
}
