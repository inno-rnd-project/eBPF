package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"netobs/internal/correlation"
)

func gpuRcaQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("node:gpu_idle_cause_weight:5m",
			sample(0.7, "node", "gpu", "cause", "memory_pressure"),
			sample(0.2, "node", "gpu", "cause", "network_pressure")).
		on("node:gpu_idle_dominant_cause:5m", sample(1.0, "node", "gpu", "cause", "memory_pressure")).
		on("kube_pod_info",
			sample(1, "node", "gpu", "namespace", "ns1", "pod", "trainer"),
			sample(1, "node", "gpu", "namespace", "ns1", "pod", "sidecar")).
		on("ALERTS", sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "src_namespace", "ns2", "src_pod", "hog")).
		// evidence: device 사용률만 존재 (GPM 미지원 consumer GPU 재현으로 sm_occupancy 는 미등록).
		on("gpuobs_device_utilization_percent", sample(2, "node", "gpu")).
		on("netobs_retrans_events_labeled_total", sample(200, "node", "gpu")).
		on("netobs_tcp_state_max_srtt_seconds", sample(0.368, "node", "gpu"))
}

// TestNodeGpuRca 는 dominant cause 와 신뢰도 (top1-top2 margin), noisy-neighbor/cross-node suspect
// 집계, narrative 합성을 검증한다. victim 이 이 노드에 사는 noisy-neighbor 만 잡히고, cross-node 는
// victim_node 매칭으로 잡힌다.
func TestNodeGpuRca(t *testing.T) {
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{
		// victim trainer 는 gpu 노드 pod → 채택. suspect hog 에 alert 매칭 (ns2/hog).
		{
			Victim:    correlation.PodIdentity{Namespace: "ns1", Pod: "trainer"},
			Suspect:   correlation.PodIdentity{Namespace: "ns2", Pod: "hog"},
			Dimension: correlation.DimensionNetwork, Score: 0.88,
		},
		// victim other 는 gpu 노드에 없음 → 제외.
		{
			Victim:    correlation.PodIdentity{Namespace: "ns9", Pod: "other"},
			Suspect:   correlation.PodIdentity{Namespace: "ns9", Pod: "x"},
			Dimension: correlation.DimensionCPU, Score: 0.99,
		},
	}}
	cn := &fakeCrossNode{data: []correlation.NodeInterference{
		{VictimNode: "gpu", SuspectNode: "worker1", Dimension: correlation.DimensionNetwork, Score: 0.5},
		{VictimNode: "worker2", SuspectNode: "worker1", Dimension: correlation.DimensionCPU, Score: 0.95}, // 다른 victim_node → 제외
	}}
	h := NewSynthesisHandler(gpuRcaQuerier(), nb, cn)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeGpuRcaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.DominantCause != "memory_pressure" {
		t.Errorf("dominant=%q want memory_pressure", resp.DominantCause)
	}
	// 신뢰도 = top1(0.7) - top2(0.2) = 0.5.
	if resp.Confidence < 0.499 || resp.Confidence > 0.501 {
		t.Errorf("confidence=%v want ~0.5 (margin)", resp.Confidence)
	}
	// suspect 는 noisy-neighbor(hog, 0.88) + cross-node(worker1, 0.5), score 내림차순.
	if len(resp.Suspects) != 2 {
		t.Fatalf("suspects=%d want 2: %+v", len(resp.Suspects), resp.Suspects)
	}
	if resp.Suspects[0].Source != "noisy_neighbor" || resp.Suspects[0].Pod != "hog" {
		t.Errorf("suspects[0]=%+v want noisy_neighbor hog", resp.Suspects[0])
	}
	if len(resp.Suspects[0].Issues) != 1 || resp.Suspects[0].Issues[0] != "NetObsDropBurst" {
		t.Errorf("suspects[0] issues=%v want [NetObsDropBurst]", resp.Suspects[0].Issues)
	}
	if resp.Suspects[1].Source != "cross_node" || resp.Suspects[1].Node != "worker1" {
		t.Errorf("suspects[1]=%+v want cross_node worker1", resp.Suspects[1])
	}
}

// TestNodeGpuRca_DedupSuspect 는 동일 suspect pod 가 dimension/signal 별로 여러 페어를 가질 때
// 후보당 1 건 (최고 score) 으로 집계되는지 검증한다.
func TestNodeGpuRca_DedupSuspect(t *testing.T) {
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{
		{Victim: correlation.PodIdentity{Namespace: "ns1", Pod: "trainer"}, Suspect: correlation.PodIdentity{Namespace: "ns2", Pod: "hog"}, Dimension: correlation.DimensionNetwork, Score: 0.3},
		{Victim: correlation.PodIdentity{Namespace: "ns1", Pod: "trainer"}, Suspect: correlation.PodIdentity{Namespace: "ns2", Pod: "hog"}, Dimension: correlation.DimensionMemory, Score: 0.8},
		{Victim: correlation.PodIdentity{Namespace: "ns1", Pod: "sidecar"}, Suspect: correlation.PodIdentity{Namespace: "ns2", Pod: "hog"}, Dimension: correlation.DimensionCPU, Score: 0.5},
	}}
	h := NewSynthesisHandler(gpuRcaQuerier(), nb, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Suspects) != 1 {
		t.Fatalf("suspects=%d want 1 (동일 suspect pod dedup): %+v", len(resp.Suspects), resp.Suspects)
	}
	if resp.Suspects[0].Pod != "hog" || resp.Suspects[0].Score != 0.8 {
		t.Errorf("suspect=%+v want hog 최고 score 0.8", resp.Suspects[0])
	}
}

// TestNodeGpuRca_NamespaceAware 는 동명 pod 가 다른 namespace 에 있을 때 victim 매칭이 namespace 를
// 함께 대조해 오귀속하지 않는지 검증한다. kube_pod_info 는 ns1/trainer 만 이 노드에 두고, noisy-
// neighbor victim 은 ns-other/trainer (동명, 다른 ns) 라 제외돼야 한다.
func TestNodeGpuRca_NamespaceAware(t *testing.T) {
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{
		{
			Victim:    correlation.PodIdentity{Namespace: "ns-other", Pod: "trainer"},
			Suspect:   correlation.PodIdentity{Namespace: "ns2", Pod: "hog"},
			Dimension: correlation.DimensionNetwork, Score: 0.9,
		},
	}}
	h := NewSynthesisHandler(gpuRcaQuerier(), nb, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Suspects) != 0 {
		t.Errorf("suspects=%+v want 0 (ns-other/trainer 는 이 노드 pod 아님)", resp.Suspects)
	}
}

// TestNodeGpuRca_DeviceScope 는 gpu 파라미터 (UUID) 가 exact gpu_uuid 매처로 evidence 쿼리를 좁히고,
// 응답에 echo 되며 narrative 주어에 device 가 붙는지 검증한다. sm_occupancy 미수집 (consumer GPU) 시
// SM active 는 생략되고 device 사용률만 evidence 에 실린다.
func TestNodeGpuRca_DeviceScope(t *testing.T) {
	q := gpuRcaQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu&gpu=GPU-abc12", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeGpuRcaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !q.sawQuery(`gpu_uuid="GPU-abc12"`) {
		t.Errorf("gpu_uuid exact 매처 미사용: %v", q.queries)
	}
	if resp.Gpu != "GPU-abc12" {
		t.Errorf("gpu=%q want GPU-abc12 (echo)", resp.Gpu)
	}
	if resp.Evidence.GpuUtilizationPercent == nil || *resp.Evidence.GpuUtilizationPercent != 2 {
		t.Errorf("gpu_utilization=%v want 2", resp.Evidence.GpuUtilizationPercent)
	}
	if resp.Evidence.SMActivePercent != nil {
		t.Errorf("sm_active=%v want nil (GPM 미수집)", resp.Evidence.SMActivePercent)
	}
	if !strings.Contains(resp.Narrative, "device GPU-abc12") {
		t.Errorf("narrative=%q want device 주어 포함", resp.Narrative)
	}
}

// TestNodeGpuRca_GpuIndexParam 은 십진 숫자 gpu 값이 gpu_index exact 매처로 매칭되는지 검증한다.
func TestNodeGpuRca_GpuIndexParam(t *testing.T) {
	q := gpuRcaQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu&gpu=0", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !q.sawQuery(`gpu_index="0"`) {
		t.Errorf("gpu_index exact 매처 미사용: %v", q.queries)
	}
}

// TestNodeGpuRca_InvalidGpu 는 형식 위반 gpu 값이 PromQL 결합 전에 400 으로 거부되는지 검증한다.
func TestNodeGpuRca_InvalidGpu(t *testing.T) {
	q := gpuRcaQuerier()
	q.queries = nil
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	target := "/api/v1/gpu-rca?node=gpu&gpu=" + url.QueryEscape(`u1"} or up{`)
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v", q.queries)
	}
}

// TestNodeGpuRca_UnknownGpu 는 미등록 device 조회 시 오류 없이 evidence 필드만 생략되는지 검증한다.
func TestNodeGpuRca_UnknownGpu(t *testing.T) {
	q := (&fakeQuerier{}).on("node:gpu_idle:5m", sample(0.9, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu&gpu=GPU-none", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 (graceful)", rec.Code)
	}
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Evidence.GpuUtilizationPercent != nil || resp.Evidence.SMActivePercent != nil {
		t.Errorf("evidence=%+v want 전 필드 생략 (미등록 device)", resp.Evidence)
	}
}

// TestNodeGpuRca_CauseRegistry 는 #287 의 cause 레지스트리 동작을 검증한다. causes 에 한국어 설명이
// 채워지고, dominant (memory_pressure) 의 차원 맞춤 evidence 가 최우선 suspect pod 스코프의 2차
// 조회로 실린다.
func TestNodeGpuRca_CauseRegistry(t *testing.T) {
	q := gpuRcaQuerier().
		on("pod:memory_pressure_score:5m", sample(0.97, "node", "gpu")).
		on("node:memory_pressure_score:5m", sample(0.58, "node", "gpu"))
	nb := &fakeNeighbors{data: []correlation.NoisyNeighbor{{
		Victim:    correlation.PodIdentity{Namespace: "ns1", Pod: "trainer"},
		Suspect:   correlation.PodIdentity{Namespace: "ns2", Pod: "hog"},
		Dimension: correlation.DimensionMemory, Score: 0.9,
	}}}
	h := NewSynthesisHandler(q, nb, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// causes 의 한국어 설명과 인과 체인 (레지스트리 유래, #303 구조 노출).
	for _, c := range resp.Causes {
		if c.Description == "" {
			t.Errorf("cause %s 의 description 이 비어 있음", c.Cause)
		}
		if c.Chain == "" {
			t.Errorf("cause %s 의 chain 이 비어 있음", c.Cause)
		}
	}
	// dominant memory_pressure 의 suspect 스코프 2차 조회.
	if !q.sawQuery(`src_pod="hog"`) {
		t.Errorf("suspect 스코프 memory 조회 미실행: %v", q.queries)
	}
	if resp.Evidence.SuspectMemoryLimitRatio == nil || *resp.Evidence.SuspectMemoryLimitRatio != 0.97 {
		t.Errorf("suspect_memory_limit_ratio=%v want 0.97", resp.Evidence.SuspectMemoryLimitRatio)
	}
	if resp.Evidence.NodeMemoryUsedRatio == nil || *resp.Evidence.NodeMemoryUsedRatio != 0.58 {
		t.Errorf("node_memory_used_ratio=%v want 0.58", resp.Evidence.NodeMemoryUsedRatio)
	}
}

// TestNodeGpuRca_ThermalEvidence 는 dominant thermal 의 온도와 slowdown 여유 산출을 검증한다.
func TestNodeGpuRca_ThermalEvidence(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("node:gpu_idle_cause_weight:5m", sample(0.8, "node", "gpu", "cause", "thermal")).
		on("node:gpu_idle_dominant_cause:5m", sample(1.0, "node", "gpu", "cause", "thermal")).
		on("gpuobs_device_temperature_celsius", sample(86, "node", "gpu")).
		on("gpuobs_device_temperature_threshold_celsius", sample(93, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Evidence.TemperatureCelsius == nil || *resp.Evidence.TemperatureCelsius != 86 {
		t.Errorf("temperature=%v want 86", resp.Evidence.TemperatureCelsius)
	}
	if resp.Evidence.SlowdownHeadroomCelsius == nil || *resp.Evidence.SlowdownHeadroomCelsius != 7 {
		t.Errorf("slowdown_headroom=%v want 7 (93-86)", resp.Evidence.SlowdownHeadroomCelsius)
	}
}

// TestNodeGpuRca_ThermalEvidenceDeviceScope 는 gpu 파라미터가 있으면 thermal evidence 의 device
// 차원 조회가 그 device 로 좁혀지는지 검증한다 (#280 evidence 계약 정합).
func TestNodeGpuRca_ThermalEvidenceDeviceScope(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("node:gpu_idle_cause_weight:5m", sample(0.8, "node", "gpu", "cause", "thermal")).
		on("node:gpu_idle_dominant_cause:5m", sample(1.0, "node", "gpu", "cause", "thermal")).
		on("gpuobs_device_temperature_celsius", sample(86, "node", "gpu")).
		on("gpuobs_device_temperature_threshold_celsius", sample(93, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu&gpu=GPU-abc12", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !q.sawQuery(`gpuobs_device_temperature_celsius{node="gpu",gpu_uuid="GPU-abc12"}`) {
		t.Errorf("thermal 온도 조회에 device 매처 미적용: %v", q.queries)
	}
	if !q.sawQuery(`gpuobs_device_temperature_threshold_celsius{node="gpu",gpu_uuid="GPU-abc12",threshold="slowdown"}`) {
		t.Errorf("thermal 임계 조회에 device 매처 미적용: %v", q.queries)
	}
}

// TestNodeGpuRca_NoEvidenceWhenNotIdle 은 dominant 부재 (유휴 게이팅 미충족) 시 2차 조회가 실행되지
// 않는지 검증한다.
func TestNodeGpuRca_NoEvidenceWhenNotIdle(t *testing.T) {
	q := (&fakeQuerier{}).on("node:gpu_idle:5m", sample(0.2, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	for _, seen := range q.queries {
		if strings.Contains(seen, "pod:memory_pressure_score:5m") || strings.Contains(seen, "temperature") {
			t.Errorf("dominant 부재인데 2차 조회 실행됨: %v", q.queries)
		}
	}
}

// TestNodeGpuRca_EvidenceFusion 은 evidence 수치 (SM active 우선, 재전송 rate, 최대 RTT) 가
// narrative 에 융합되고 network 계열 dominant cause 에 인과 체인 문구가 붙는지 검증한다.
func TestNodeGpuRca_EvidenceFusion(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("node:gpu_idle_cause_weight:5m", sample(0.8, "node", "gpu", "cause", "network_pressure")).
		on("node:gpu_idle_dominant_cause:5m", sample(1.0, "node", "gpu", "cause", "network_pressure")).
		on("gpuobs_device_utilization_percent", sample(7, "node", "gpu")).
		on("gpuobs_device_gpm_utilization_percent", sample(2, "node", "gpu")).
		on("netobs_retrans_events_labeled_total", sample(200, "node", "gpu")).
		on("netobs_tcp_state_max_srtt_seconds", sample(0.368, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Evidence.RetransPerSec == nil || *resp.Evidence.RetransPerSec != 200 {
		t.Errorf("retrans=%v want 200", resp.Evidence.RetransPerSec)
	}
	if resp.Evidence.MaxSrttSeconds == nil || *resp.Evidence.MaxSrttSeconds != 0.368 {
		t.Errorf("max_srtt=%v want 0.368", resp.Evidence.MaxSrttSeconds)
	}
	// SM active 가 있으면 device 사용률보다 우선한다.
	want := "근거 SM active 2.0%인데 재전송 200.0/s·RTT 368ms"
	if !strings.Contains(resp.Narrative, want) {
		t.Errorf("narrative=%q want %q 포함", resp.Narrative, want)
	}
	if !strings.Contains(resp.Narrative, "인과 체인: 재전송 → 통신 블로킹 → GPU 대기") {
		t.Errorf("narrative=%q want 인과 체인 문구 (network_pressure)", resp.Narrative)
	}
}

// TestNodeGpuRca_CauseNarrative 는 #287 의 레지스트리 기반 narrative 를 검증한다. dominant slug 에
// 한국어 설명이 부연되고, memory 계열 인과 체인이 붙으며, GPM 미지원 시 GPU 축이 device 사용률로
// fallback 하고, 차원 맞춤 수치가 근거에 융합된다.
func TestNodeGpuRca_CauseNarrative(t *testing.T) {
	q := gpuRcaQuerier().
		on("pod:memory_pressure_score:5m", sample(0.97, "node", "gpu")).
		on("node:memory_pressure_score:5m", sample(0.58, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Narrative, "memory_pressure(working set 의 memory limit 근접") {
		t.Errorf("narrative=%q want 설명 부연", resp.Narrative)
	}
	if !strings.Contains(resp.Narrative, "인과 체인: 메모리 reclaim/stall") {
		t.Errorf("narrative=%q want memory 인과 체인", resp.Narrative)
	}
	if !strings.Contains(resp.Narrative, "GPU 사용률 2.0%") {
		t.Errorf("narrative=%q want GPU 사용률 fallback (GPM 미수집)", resp.Narrative)
	}
	if !strings.Contains(resp.Narrative, "노드 메모리 사용률 58%") || !strings.Contains(resp.Narrative, "working_set/limit 97%") {
		t.Errorf("narrative=%q want memory 차원 수치 융합", resp.Narrative)
	}
	// 신뢰도 0.5 (0.7-0.2) 는 백중 아님 → 판정 유보 문구 없음.
	if strings.Contains(resp.Narrative, "판정 유보") {
		t.Errorf("narrative=%q want 판정 유보 없음 (margin 0.5)", resp.Narrative)
	}
}

// TestNodeGpuRca_NcclChain 은 #303 의 nccl_collective_stall chain 정정을 검증한다. network_pressure
// 와 문구가 달라야 하고 (collective 동기화 대기 축), causes[].chain 으로 구조 노출된다.
func TestNodeGpuRca_NcclChain(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("node:gpu_idle_cause_weight:5m", sample(0.8, "node", "gpu", "cause", "nccl_collective_stall")).
		on("node:gpu_idle_dominant_cause:5m", sample(1.0, "node", "gpu", "cause", "nccl_collective_stall"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := "collective 동기화 대기 → rank 정체 → GPU 대기"
	if len(resp.Causes) != 1 || resp.Causes[0].Chain != want {
		t.Errorf("causes=%+v want chain %q", resp.Causes, want)
	}
	if !strings.Contains(resp.Narrative, want) {
		t.Errorf("narrative=%q want 정정된 nccl chain", resp.Narrative)
	}
}

// TestNodeGpuRca_AmbiguousNarrative 는 top1 과 top2 가 백중 (margin < 0.1) 일 때 판정 유보 문구와
// top2 cause 가 narrative 에 실리는지 검증한다.
func TestNodeGpuRca_AmbiguousNarrative(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:gpu_idle:5m", sample(0.9, "node", "gpu")).
		on("node:gpu_idle_cause_weight:5m",
			sample(0.45, "node", "gpu", "cause", "memory_pressure"),
			sample(0.40, "node", "gpu", "cause", "cpu_throttle")).
		on("node:gpu_idle_dominant_cause:5m", sample(1.0, "node", "gpu", "cause", "memory_pressure"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Narrative, "top2 cpu_throttle 와 백중이라 판정 유보") {
		t.Errorf("narrative=%q want 판정 유보 문구", resp.Narrative)
	}
}

// TestNodeGpuRca_MissingNode 는 node 파라미터 누락 시 400 을 검증한다.
func TestNodeGpuRca_MissingNode(t *testing.T) {
	h := NewSynthesisHandler(gpuRcaQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (node 누락)", rec.Code)
	}
}

// TestNodeGpuRca_InvalidNode 는 DNS-1123 위반 node 값이 PromQL 결합 전에 400 으로 거부되는지 검증한다.
func TestNodeGpuRca_InvalidNode(t *testing.T) {
	q := gpuRcaQuerier()
	q.queries = nil
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	target := "/api/v1/gpu-rca?node=" + url.QueryEscape(`gpu"} or up{`)
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v (PromQL 결합 전 차단이어야 함)", q.queries)
	}
}

// TestNodeGpuRca_NotIdle 은 cause 귀속이 비면 (유휴 게이팅 미충족) narrative 가 그 사실을 적고
// confidence 가 0 인지 검증한다.
func TestNodeGpuRca_NotIdle(t *testing.T) {
	q := (&fakeQuerier{}).on("node:gpu_idle:5m", sample(0.2, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.DominantCause != "" || resp.Confidence != 0 {
		t.Errorf("resp=%+v want dominant 없음/confidence 0", resp)
	}
	if !strings.Contains(resp.Narrative, "임계(idle>0.5) 미만") {
		t.Errorf("narrative=%q want 게이팅 미충족 문구", resp.Narrative)
	}
}

// TestNodeGpuRca_IdleNoCause 는 유휴 (idle > 0.5) 인데 rise 부재로 cause weight 가 비는 평시 상태
// (#285 이후) 에 게이팅 미만 문구가 아니라 "귀속할 cause 없음" 문구가 나오는지 검증한다.
func TestNodeGpuRca_IdleNoCause(t *testing.T) {
	q := (&fakeQuerier{}).on("node:gpu_idle:5m", sample(1.0, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeGpuRca(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-rca?node=gpu", nil))
	var resp NodeGpuRcaResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Narrative, "신규 압박 rise 가 없어 귀속할 cause 없음") {
		t.Errorf("narrative=%q want rise 부재 문구", resp.Narrative)
	}
	if strings.Contains(resp.Narrative, "임계(idle>0.5) 미만") {
		t.Errorf("narrative=%q want 게이팅 미만 문구 없음 (idle 1.0 모순)", resp.Narrative)
	}
}
