package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNodePods 는 #330 의 노드 스코프 pod 사용량 합성을 검증한다. limit 있는 pod 는 percent 와
// 절대량이 공존하고, limit 없는 pod 는 percent 생략과 절대량 표현 (#328 규약), 종료 pod 는
// severity 와 미관측 사유 생략, 관측 없는 실행 pod 는 no_data 사유가 붙는다.
func TestNodePods(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info",
			sample(1, "namespace", "ns1", "pod", "limited", "uid", "u-1", "node", "n1", "pod_ip", "10.0.0.1", "host_ip", "192.168.1.10"),
			sample(1, "namespace", "ns1", "pod", "limitless", "uid", "u-2", "node", "n1", "pod_ip", "10.0.0.2", "host_ip", "192.168.1.10"),
			sample(1, "namespace", "ns2", "pod", "done", "uid", "u-3", "node", "n1", "pod_ip", "10.0.0.3", "host_ip", "192.168.1.10"),
			sample(1, "namespace", "ns2", "pod", "silent", "uid", "u-4", "node", "n1", "pod_ip", "10.0.0.4", "host_ip", "192.168.1.10")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u-1", "phase", "Running"),
			sample(1, "uid", "u-2", "phase", "Running"),
			sample(1, "uid", "u-3", "phase", "Succeeded"),
			sample(1, "uid", "u-4", "phase", "Running")).
		on("rate(container_cpu_usage_seconds_total",
			sample(0.5, "namespace", "ns1", "pod", "limited"),
			sample(0.042, "namespace", "ns1", "pod", "limitless")).
		on("container_memory_working_set_bytes",
			sample(2e8, "namespace", "ns1", "pod", "limited"),
			sample(5e7, "namespace", "ns1", "pod", "limitless")).
		on(`resource="cpu"`, sample(2, "namespace", "ns1", "pod", "limited")).
		on(`resource="memory"`, sample(4e8, "namespace", "ns1", "pod", "limited")).
		on("netobs_pod_bytes_total",
			sample(1024, "src_namespace", "ns1", "src_pod", "limited"),
			sample(64, "src_namespace", "ns1", "src_pod", "limitless")).
		on("pod:cpu_throttle_score:5m", sample(0.8, "src_namespace", "ns1", "src_pod", "limited")).
		on("pod:memory_pressure_score:5m", sample(0.2, "src_namespace", "ns1", "src_pod", "limited")).
		on("netobs_bpf_program_loaded", sample(26, "node", "n1"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodePods(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/n1/pods", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodePodsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pods) != 4 {
		t.Fatalf("pods=%d want 4", len(resp.Pods))
	}
	// namespace 와 pod 사전순 정렬.
	order := []string{"limited", "limitless", "done", "silent"}
	for i, want := range order {
		if resp.Pods[i].Pod != want {
			t.Fatalf("정렬 순서=%v want %v", resp.Pods, order)
		}
	}

	limited := resp.Pods[0]
	if limited.CPUUsageCores == nil || *limited.CPUUsageCores != 0.5 || limited.CPUPercent == nil || *limited.CPUPercent != 25 {
		t.Errorf("limited cpu=%+v want cores 0.5 / percent 25 (0.5/2)", limited)
	}
	if limited.MemoryPercent == nil || *limited.MemoryPercent != 50 || limited.MemoryWorkingSetBytes == nil {
		t.Errorf("limited memory=%+v want percent 50 (2e8/4e8)", limited)
	}
	if limited.NetworkBytesPerSec == nil || *limited.NetworkBytesPerSec != 1024 {
		t.Errorf("limited network=%v want 1024", limited.NetworkBytesPerSec)
	}
	// severity 는 pressure 3종 최대 (0.8) 의 환산이다.
	if limited.Severity != "high" {
		t.Errorf("limited severity=%q want high (max 0.8)", limited.Severity)
	}
	if limited.UnobservedReason != "" {
		t.Errorf("limited reason=%q want 생략 (관측됨)", limited.UnobservedReason)
	}

	limitless := resp.Pods[1]
	if limitless.CPUPercent != nil || limitless.MemoryPercent != nil {
		t.Errorf("limitless percent=%+v want 생략 (limit 미설정)", limitless)
	}
	if limitless.CPUUsageCores == nil || *limitless.CPUUsageCores != 0.042 || limitless.MemoryWorkingSetBytes == nil {
		t.Errorf("limitless 절대량=%+v want cores 0.042 / working set", limitless)
	}

	done := resp.Pods[2]
	if done.Phase != "Succeeded" || done.UnobservedReason != "" || done.Severity != "" {
		t.Errorf("done=%+v want Succeeded / 사유·severity 생략 (#314)", done)
	}

	silent := resp.Pods[3]
	if silent.UnobservedReason != "no_data" {
		t.Errorf("silent reason=%q want no_data (agent 있는 노드의 무관측 실행 pod)", silent.UnobservedReason)
	}

	// 쿼리 규약: pod-level cgroup 행 가드와 limits 의 KSM 중복 dedup (max by 후 합산).
	if !q.sawQuery(`container="",pod!=""`) {
		t.Error(`cadvisor 합산에 pod-level 행 가드 (container="") 부재`)
	}
	if !q.sawQuery("max by(namespace, pod, container) (kube_pod_container_resource_limits") {
		t.Error("limits 합산에 KSM 롤링 중복 dedup (max by) 부재")
	}
}

// TestNodePods_AgentAbsent 는 agent 미배치 노드의 실행 pod 전부에 agent_absent 사유가 붙는지
// 검증한다 (#320 규약 재사용).
func TestNodePods_AgentAbsent(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info",
			sample(1, "namespace", "kube-system", "pod", "etcd-master", "uid", "u-9", "node", "master", "pod_ip", "192.168.1.1", "host_ip", "192.168.1.1")).
		on("kube_pod_status_phase", sample(1, "uid", "u-9", "phase", "Running"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodePods(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/master/pods", nil))
	var resp NodePodsResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Pods) != 1 || resp.Pods[0].UnobservedReason != "agent_absent" {
		t.Errorf("pods=%+v want agent_absent (netobs_bpf_program_loaded 부재)", resp.Pods)
	}
	if !strings.Contains(resp.Summary, "미관측 1") {
		t.Errorf("summary=%q want 미관측 집계", resp.Summary)
	}
}

// TestNodePods_InvalidPath 는 노드명 검증 실패와 빈 노드가 쿼리 실행 전에 400 으로 거부되는지
// 검증한다.
func TestNodePods_InvalidPath(t *testing.T) {
	q := &fakeQuerier{}
	h := NewSynthesisHandler(q, nil, nil)
	for _, target := range []string{"/api/v1/node/UPPER_case/pods", "/api/v1/node/-bad-/pods"} {
		rec := httptest.NewRecorder()
		h.GetNodePods(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d want 400", target, rec.Code)
		}
	}
	if len(q.queries) != 0 {
		t.Errorf("거부 후 쿼리 실행됨: %v", q.queries)
	}
}

// TestNodePods_LimitlessPartialSeverity 는 #378 회귀 가드다. cpu·memory limit 이 없어 두 pressure
// score 를 산출할 수 없고 network 만 low 인 pod 는 network 단독으로 low 를 단정하지 않고 partial 로
// 구분한다. limit 이 모두 있는 pod 는 종전대로 low 이고, limit 이 없어도 network 가 elevated 면 실제
// 압박이라 elevated 를 그대로 노출한다.
func TestNodePods_LimitlessPartialSeverity(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info",
			sample(1, "namespace", "ns1", "pod", "netlow", "uid", "u-1", "node", "n1", "pod_ip", "10.0.0.1", "host_ip", "192.168.1.10"),
			sample(1, "namespace", "ns1", "pod", "full", "uid", "u-2", "node", "n1", "pod_ip", "10.0.0.2", "host_ip", "192.168.1.10"),
			sample(1, "namespace", "ns1", "pod", "netelev", "uid", "u-3", "node", "n1", "pod_ip", "10.0.0.3", "host_ip", "192.168.1.10")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u-1", "phase", "Running"),
			sample(1, "uid", "u-2", "phase", "Running"),
			sample(1, "uid", "u-3", "phase", "Running")).
		on("rate(container_cpu_usage_seconds_total",
			sample(0.05, "namespace", "ns1", "pod", "netlow"),
			sample(0.05, "namespace", "ns1", "pod", "full"),
			sample(0.05, "namespace", "ns1", "pod", "netelev")).
		on("container_memory_working_set_bytes",
			sample(5e7, "namespace", "ns1", "pod", "netlow"),
			sample(5e7, "namespace", "ns1", "pod", "full"),
			sample(5e7, "namespace", "ns1", "pod", "netelev")).
		// limit 은 full pod 만 보유한다.
		on(`resource="cpu"`, sample(2, "namespace", "ns1", "pod", "full")).
		on(`resource="memory"`, sample(4e8, "namespace", "ns1", "pod", "full")).
		on("netobs_pod_bytes_total",
			sample(64, "src_namespace", "ns1", "src_pod", "netlow"),
			sample(64, "src_namespace", "ns1", "src_pod", "full"),
			sample(64, "src_namespace", "ns1", "src_pod", "netelev")).
		on("pod:cpu_throttle_score:5m", sample(0.0001, "src_namespace", "ns1", "src_pod", "full")).
		on("pod:memory_pressure_score:5m", sample(0.0001, "src_namespace", "ns1", "src_pod", "full")).
		on("pod:network_pressure_score:5m",
			sample(0.0001, "src_namespace", "ns1", "src_pod", "netlow"),
			sample(0.0001, "src_namespace", "ns1", "src_pod", "full"),
			sample(0.5, "src_namespace", "ns1", "src_pod", "netelev")).
		on("netobs_bpf_program_loaded", sample(26, "node", "n1"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodePods(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/n1/pods", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodePodsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sev := map[string]string{}
	for _, p := range resp.Pods {
		sev[p.Pod] = p.Severity
	}
	// netlow: network 만 low + cpu·memory limit 없음 → partial (network 단독 low 단정 회피).
	if sev["netlow"] != severityPartial {
		t.Errorf("netlow severity=%q want %q (limit 없는 network 단독 low)", sev["netlow"], severityPartial)
	}
	// full: cpu·memory·network 모두 low 이고 limit 존재 → low.
	if sev["full"] != "low" {
		t.Errorf("full severity=%q want low (전 차원 측정·low)", sev["full"])
	}
	// netelev: limit 없어도 network 가 elevated 면 실제 압박이라 그대로 노출 (partial 아님).
	if sev["netelev"] != "elevated" {
		t.Errorf("netelev severity=%q want elevated (실제 network 압박은 partial 로 가리지 않음)", sev["netelev"])
	}
}
