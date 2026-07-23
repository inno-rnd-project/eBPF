package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func overviewFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		// gpu 노드는 kube-state-metrics 롤링 업데이트 중복 시계열 (instance 상이) 을 재현해 dedup 을
		// 가드한다. 중복이 계상되면 아래 Total/GPU 단정이 깨진다.
		on("kube_node_info",
			sample(1, "node", "gpu", "instance", "ksm-a"),
			sample(1, "node", "gpu", "instance", "ksm-b"),
			sample(1, "node", "worker1"),
			sample(1, "node", "worker2"),
			sample(1, "node", "deadnode")).
		on("kube_node_status_condition",
			sample(1, "node", "gpu", "condition", "Ready", "status", "true"),
			sample(1, "node", "worker1", "condition", "Ready", "status", "true"),
			sample(1, "node", "worker2", "condition", "Ready", "status", "true")).
		// firing alert: gpu 노드 경고 + issues 집계 (동일 alertname 2 시리즈는 1건으로 dedup).
		on("ALERTS",
			sample(1, "alertname", "GPUIdleWithMemoryPressure", "severity", "warning", "node", "gpu", "pod", "trainer"),
			sample(1, "alertname", "GPUIdleWithMemoryPressure", "severity", "warning", "node", "gpu", "pod", "other"),
			sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "src_pod", "trainer")).
		// worker1 은 압박 elevated 초과 (0.5 > 0.4) 로 경고.
		on("node:pressure_score:5m",
			sample(0.5, "node", "worker1"),
			sample(0.1, "node", "worker2")).
		on("kube_pod_info",
			sample(1, "namespace", "ns1", "pod", "trainer", "node", "gpu"),
			sample(1, "namespace", "ns1", "pod", "idle", "node", "worker1"),
			sample(1, "namespace", "ns2", "pod", "web", "node", "worker2")).
		on("netobs_bpf_program_loaded",
			sample(26, "node", "gpu"), sample(26, "node", "worker1"), sample(26, "node", "worker2")).
		on("netobs_pod_bytes_total", sample(3, "src_namespace", "ns1", "src_pod", "trainer")).
		on("kube_node_status_capacity",
			sample(2, "node", "gpu", "resource", "nvidia_com_gpu", "instance", "ksm-a"),
			sample(2, "node", "gpu", "resource", "nvidia_com_gpu", "instance", "ksm-b")).
		on("cluster:cpu_health_score", sample(0.9)).
		on("cluster:gpu_health_score", sample(0.23)).
		on("cluster:memory_health_score", sample(0.8)).
		on("cluster:network_health_score", sample(0.95))
}

// TestOverview 는 카드 5장 (노드 3단, pod 커버리지, issues dedup, GPU fleet, weakest) 의 합성 판정을
// 검증한다.
// TestOverview_TerminatedPodsExcluded 는 #314 의 종료 pod 집계를 검증한다. Succeeded 와 Failed 는
// terminated 로 세고 no_data 분모에서 제외되며, 실행 중 미관측 pod 만 no_data 로 남는다. node-map
// 의 completed (Succeeded 한정) 와 달리 상위 집합이라 terminated 로 명명한다.
func TestOverview_TerminatedPodsExcluded(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info",
			sample(1, "namespace", "ns1", "pod", "running-observed", "node", "n1"),
			sample(1, "namespace", "ns1", "pod", "running-silent", "node", "n1"),
			sample(1, "namespace", "ns1", "pod", "job-ok", "node", "n1"),
			sample(1, "namespace", "ns1", "pod", "job-fail", "node", "n1")).
		on("netobs_pod_bytes_total", sample(3, "src_namespace", "ns1", "src_pod", "running-observed")).
		on("kube_pod_status_phase",
			sample(1, "namespace", "ns1", "pod", "running-observed", "phase", "Running"),
			sample(1, "namespace", "ns1", "pod", "running-silent", "phase", "Running"),
			sample(1, "namespace", "ns1", "pod", "job-ok", "phase", "Succeeded"),
			sample(1, "namespace", "ns1", "pod", "job-fail", "phase", "Failed")).
		on("netobs_bpf_program_loaded", sample(26, "node", "n1"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	var resp OverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Pods.Total != 4 || resp.Pods.Live != 1 || resp.Pods.Terminated != 2 || resp.Pods.NoData != 1 {
		t.Errorf("pods=%+v want total 4 / live 1 / terminated 2 / no_data 1", resp.Pods)
	}
}

func TestOverview(t *testing.T) {
	h := NewSynthesisHandler(overviewFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp OverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 노드: gpu 는 alert 경고, worker1 은 압박 경고, worker2 정상, deadnode 는 Ready 부재로 down.
	if resp.Nodes != (OverviewNodes{Total: 4, Healthy: 1, Warning: 2, Down: 1}) {
		t.Errorf("nodes=%+v want total4/healthy1/warning2/down1", resp.Nodes)
	}
	// pod: trainer 만 관측.
	if resp.Pods != (OverviewPods{Total: 3, Live: 1, NoData: 2}) {
		t.Errorf("pods=%+v want total3/live1/nodata2", resp.Pods)
	}
	// issues: alertname 단위 dedup 으로 2건 (warning 1 + critical 1). 두 alert 모두 pod 계열
	// 라벨 (pod / src_pod) 이 있어 scope 는 pod 2건이다 (#326).
	if resp.Issues != (OverviewIssues{Total: 2, Critical: 1, Warning: 1, PodScope: 2}) {
		t.Errorf("issues=%+v want total2/critical1/warning1/pod2", resp.Issues)
	}
	if resp.GPU != (OverviewGPU{Nodes: 1, Devices: 2}) {
		t.Errorf("gpu=%+v want nodes1/devices2", resp.GPU)
	}
	if resp.Weakest == nil || resp.Weakest.Dimension != "gpu" || resp.Weakest.Health != 0.23 {
		t.Errorf("weakest=%+v want gpu/0.23", resp.Weakest)
	}
}

// TestOverview_AtParam 은 at 지정 시 평가 시점이 ctx 로 전파되고 generated_at 에 반영되는지 검증한다.
func TestOverview_AtParam(t *testing.T) {
	q := overviewFakeQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview?at=2026-07-09T01:00:00Z", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp OverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.GeneratedAt != "2026-07-09T01:00:00Z" {
		t.Errorf("generated_at=%q want at 반영", resp.GeneratedAt)
	}
	if q.lastAt.Format("2006-01-02T15:04:05Z") != "2026-07-09T01:00:00Z" {
		t.Errorf("lastAt=%v want ctx 전파", q.lastAt)
	}
	rec = httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview?at=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid at)", rec.Code)
	}
}

// TestOverview_NilQuerier 는 querier 미주입 시 빈 집계를 graceful 하게 돌려주는지 검증한다.
func TestOverview_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp OverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Nodes.Total != 0 || resp.Weakest != nil {
		t.Errorf("resp=%+v want 빈 집계", resp)
	}
}

// TestOverview_IssueScopeAndWatchdog 는 #326 의 issues 집계 계약을 검증한다. Watchdog heartbeat 는
// severity 무관하게 집계 전체에서 제외되고, scope 는 pod 계열 라벨이 있으면 pod, 없이 node 만
// 있으면 node, 둘 다 없으면 (cluster z-score 계열) cluster 로 단일 분류되어 total 에 additive 하다.
// 동일 alertname 의 시리즈 간 라벨이 다르면 가장 구체적인 scope 를 채택한다.
func TestOverview_IssueScopeAndWatchdog(t *testing.T) {
	q := (&fakeQuerier{}).
		on("ALERTS",
			sample(1, "alertname", "Watchdog", "severity", "none"),
			sample(1, "alertname", "GPUUtilSpikeDetected", "severity", "warning"),
			sample(1, "alertname", "GPUObsThrottleActive", "severity", "critical", "node", "gpu"),
			sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "node", "gpu", "victim_pod", "trainer"),
			// 동일 alertname 이 node 만 있는 시리즈와 pod 시리즈로 나뉘어도 pod 로 단일 분류된다.
			sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp OverviewResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	want := OverviewIssues{Total: 3, Critical: 2, Warning: 1, PodScope: 1, NodeScope: 1, ClusterScope: 1}
	if resp.Issues != want {
		t.Errorf("issues=%+v want %+v (Watchdog 제외, scope 단일 분류)", resp.Issues, want)
	}
}
