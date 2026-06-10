package registry

import (
	"sort"
	"strings"
	"testing"
)

// fakeSources 는 단위 테스트용 Sources 구현이다. 등록된 victim 키 별로 미리 준비한 neighbor 리스트
// 를 돌려준다. drop flow 는 본 mapping 셋에서 mapNetObsDropBurst 만 잠재적으로 호출 가능한데 본
// mapping 은 sources 를 사용하지 않으므로 빈 슬라이스만 준비해도 무해하다. gpuSignal 은 #122 의
// multi-source cross-reference 산출 시 GPU 신호 강도 fixture 다.
type fakeSources struct {
	neighborsByVictim map[string][]NeighborInfo
	dropFlows         []DropFlowInfo
	gpuSignal         float64
}

func (f *fakeSources) TopNeighbors(ns, pod string) []NeighborInfo {
	return f.neighborsByVictim[ns+"/"+pod]
}
func (f *fakeSources) TopDropFlows(namespace string) []DropFlowInfo {
	return f.dropFlows
}
func (f *fakeSources) GPUSignal(node string) float64 {
	return f.gpuSignal
}

// EvaluateConfidence 는 production Sources 와 동일한 가중치 식을 fixture 로 복제 한다. 단위
// 테스트 가 실제 confidence 산출 식 의 동작 을 mapping 단계 까지 추적 가능 하게 한다. 가중치
// 정합 은 sources 패키지 의 confidence_test 에서 별도 회귀 가드 한다.
func (f *fakeSources) EvaluateConfidence(neighbors []NeighborInfo, dropFlows []DropFlowInfo, gpuSignal float64) float64 {
	c := 0.0
	for _, n := range neighbors {
		s := n.Score
		if s < 0 {
			s = -s
		}
		if s > c {
			c = s
		}
	}
	if c > 1 {
		c = 1
	}
	n := 0.0
	for _, d := range dropFlows {
		if d.RatePerSec > n {
			n = d.RatePerSec
		}
	}
	n = n / 100.0
	if n > 1 {
		n = 1
	}
	if n < 0 {
		n = 0
	}
	g := gpuSignal
	if g > 1 {
		g = 1
	}
	if g < 0 {
		g = 0
	}
	return 0.5*c + 0.3*n + 0.2*g
}

// TestNew_RegistersExactlyNineMappings 는 명세상 9 alert mapping 이 정확히 등록되는지 회귀
// 가드한다. 신규 mapping 추가 시 본 테스트의 expected 셋도 함께 갱신해야 하며 의도된 변경이
// 아닌 누락 / 오타가 발견된다.
func TestNew_RegistersExactlyNineMappings(t *testing.T) {
	r := New()
	names := r.Alertnames()
	sort.Strings(names)
	want := []string{
		"CorrelationStrongNoisyNeighbor",
		"GPUIdleWithCPUThrottle",
		"GPUIdleWithHostComputeStall",
		"GPUIdleWithMemoryPressure",
		"GPUIdleWithNetworkPressure",
		"GPUIdleWithPCIeSaturation",
		"GPUObsCudaStreamWaitHigh",
		"GPUObsThermalThrottleSustained",
		"NetObsDropBurst",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("registered alertnames=%v;\n   want=%v", names, want)
	}
}

// TestDispatch_UnknownAlertReturnsFalse 는 mapping 미등록 alert 가 false 와 빈 RCASummary 를
// 돌려주는지 검증한다. 호출 측이 본 결과를 raw label echo back 으로 silent drop 회피한다.
func TestDispatch_UnknownAlertReturnsFalse(t *testing.T) {
	r := New()
	summary, ok := r.Dispatch("NonExistentAlert", map[string]string{"foo": "bar"}, nil)
	if ok {
		t.Errorf("ok=true for unknown alert; want false")
	}
	if summary.AlertName != "NonExistentAlert" {
		t.Errorf("summary.AlertName=%q; want NonExistentAlert", summary.AlertName)
	}
}

// TestDispatch_NetObsDropBurst 는 5-tuple 라벨이 primary_drop_flow 에 정확히 직렬화되는지 검증한다.
func TestDispatch_NetObsDropBurst(t *testing.T) {
	r := New()
	labels := map[string]string{
		"src_namespace": "default",
		"src_pod":       "client",
		"dst_ip":        "10.0.0.5",
		"dst_port":      "8080",
		"protocol":      "tcp",
		"drop_reason":   "TCP_RESET",
	}
	summary, ok := r.Dispatch("NetObsDropBurst", labels, nil)
	if !ok {
		t.Fatalf("ok=false; want true")
	}
	if summary.DominantDimension != "network" {
		t.Errorf("DominantDimension=%q; want network", summary.DominantDimension)
	}
	if summary.TopSuspect != "default/client" {
		t.Errorf("TopSuspect=%q; want default/client", summary.TopSuspect)
	}
	if !strings.Contains(summary.PrimaryDropFlow, "10.0.0.5:8080") {
		t.Errorf("PrimaryDropFlow=%q; want to contain 10.0.0.5:8080", summary.PrimaryDropFlow)
	}
	if !strings.Contains(summary.PrimaryDropFlow, "reason=TCP_RESET") {
		t.Errorf("PrimaryDropFlow=%q; want to contain reason=TCP_RESET", summary.PrimaryDropFlow)
	}
}

// TestDispatch_GPUObsCudaStreamWaitHighOverridesDimension 은 noisy neighbor 의 dimension 이 alert
// 의 default (gpu) 를 overwrite 하는지 검증한다. CPU 자원이 GPU stream wait 의 진짜 원인일 때
// dimension 이 cpu 로 보고되어야 운영자가 dashboard 에서 정확한 패널을 우선 본다.
func TestDispatch_GPUObsCudaStreamWaitHighOverridesDimension(t *testing.T) {
	r := New()
	sources := &fakeSources{
		neighborsByVictim: map[string][]NeighborInfo{
			"default/victim": {
				{SuspectNamespace: "noisy", SuspectPod: "hog", Dimension: "cpu", Score: 0.9},
			},
		},
	}
	labels := map[string]string{
		"src_namespace": "default",
		"src_pod":       "victim",
	}
	summary, ok := r.Dispatch("GPUObsCudaStreamWaitHigh", labels, sources)
	if !ok {
		t.Fatalf("ok=false; want true")
	}
	if summary.DominantDimension != "cpu" {
		t.Errorf("DominantDimension=%q; want cpu (overridden by neighbor)", summary.DominantDimension)
	}
	if summary.TopSuspect != "noisy/hog" {
		t.Errorf("TopSuspect=%q; want noisy/hog", summary.TopSuspect)
	}
}

// TestDispatch_GPUObsCudaStreamWaitHighFallbackToVictim 은 neighbor 가 없을 때 victim Pod 가
// top_suspect 로 fallback 되는지 검증한다.
func TestDispatch_GPUObsCudaStreamWaitHighFallbackToVictim(t *testing.T) {
	r := New()
	sources := &fakeSources{neighborsByVictim: nil}
	labels := map[string]string{
		"src_namespace": "default",
		"src_pod":       "victim",
	}
	summary, _ := r.Dispatch("GPUObsCudaStreamWaitHigh", labels, sources)
	if summary.DominantDimension != "gpu" {
		t.Errorf("DominantDimension=%q; want gpu (no neighbor override)", summary.DominantDimension)
	}
	if summary.TopSuspect != "default/victim" {
		t.Errorf("TopSuspect=%q; want default/victim", summary.TopSuspect)
	}
}

// TestDispatch_GPUIdleWithCPUThrottleHasDimensionCPU 은 수용 조건 2번 (workload-injector cpu kind
// 발화 시 dominant_dimension=cpu) 의 단위 가드다. e2e 검증의 전제가 본 unit test 로 빠르게 잡힌다.
func TestDispatch_GPUIdleWithCPUThrottleHasDimensionCPU(t *testing.T) {
	r := New()
	labels := map[string]string{
		"src_namespace": "perf",
		"src_pod":       "stress-cpu",
		"node":          "gpu",
	}
	summary, ok := r.Dispatch("GPUIdleWithCPUThrottle", labels, nil)
	if !ok {
		t.Fatalf("ok=false")
	}
	if summary.DominantDimension != "cpu" {
		t.Errorf("DominantDimension=%q; want cpu", summary.DominantDimension)
	}
	if summary.TopSuspect != "perf/stress-cpu" {
		t.Errorf("TopSuspect=%q; want perf/stress-cpu", summary.TopSuspect)
	}
}

// TestDispatch_CorrelationStrongNoisyNeighborUsesResourceDimension 은 alert 라벨의
// resource_dimension 이 dominant_dimension 으로 그대로 흐르는지 검증한다.
func TestDispatch_CorrelationStrongNoisyNeighborUsesResourceDimension(t *testing.T) {
	r := New()
	labels := map[string]string{
		"victim_namespace":   "default",
		"victim_pod":         "v",
		"suspect_namespace":  "noisy",
		"suspect_pod":        "s",
		"resource_dimension": "gpu",
	}
	summary, _ := r.Dispatch("CorrelationStrongNoisyNeighbor", labels, nil)
	if summary.DominantDimension != "gpu" {
		t.Errorf("DominantDimension=%q; want gpu", summary.DominantDimension)
	}
	if summary.TopSuspect != "noisy/s" {
		t.Errorf("TopSuspect=%q; want noisy/s", summary.TopSuspect)
	}
}

// TestDispatch_GPUObsThermalThrottleNodeIdentity 는 pod 식별이 불가한 node-level alert 가
// top_suspect 를 node 식별자로 채우는지 검증한다.
func TestDispatch_GPUObsThermalThrottleNodeIdentity(t *testing.T) {
	r := New()
	labels := map[string]string{
		"node":     "gpu",
		"gpu_uuid": "GPU-1234",
	}
	summary, _ := r.Dispatch("GPUObsThermalThrottleSustained", labels, nil)
	if !strings.Contains(summary.TopSuspect, "node/gpu") {
		t.Errorf("TopSuspect=%q; want to contain node/gpu", summary.TopSuspect)
	}
	if !strings.Contains(summary.TopSuspect, "GPU-1234") {
		t.Errorf("TopSuspect=%q; want to contain GPU-1234", summary.TopSuspect)
	}
	if summary.DominantDimension != "thermal" {
		t.Errorf("DominantDimension=%q; want thermal", summary.DominantDimension)
	}
}
