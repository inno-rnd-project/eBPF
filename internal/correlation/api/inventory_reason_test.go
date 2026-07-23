package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPods_UnobservedReason 은 #320 의 미관측 사유 분류를 검증한다. agent 미배치 노드의 pod 는
// hostNetwork 여부와 무관하게 agent_absent, agent 노드의 hostNetwork pod 는 host_network, 관측
// 가능한데 시리즈가 없으면 no_data, 관측 성공과 종료 pod 는 사유 생략이다.
func TestPods_UnobservedReason(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info",
			// master: agent 미배치 + hostNetwork (static pod 패턴) → agent_absent 우선.
			sample(1, "namespace", "kube-system", "pod", "apiserver", "uid", "u1", "node", "master", "pod_ip", "10.0.0.1", "host_ip", "10.0.0.1"),
			// worker: hostNetwork (cilium 패턴) → host_network.
			sample(1, "namespace", "kube-system", "pod", "cilium-x", "uid", "u2", "node", "worker", "pod_ip", "10.0.0.2", "host_ip", "10.0.0.2"),
			// worker: 관측 가능 + 시리즈 부재 → no_data.
			sample(1, "namespace", "app", "pod", "silent", "uid", "u3", "node", "worker", "pod_ip", "172.16.0.5", "host_ip", "10.0.0.2"),
			// worker: 관측 성공 → 사유 생략.
			sample(1, "namespace", "app", "pod", "seen", "uid", "u4", "node", "worker", "pod_ip", "172.16.0.6", "host_ip", "10.0.0.2"),
			// worker: 종료 pod → 사유 생략 (#314 규약).
			sample(1, "namespace", "app", "pod", "done", "uid", "u5", "node", "worker", "pod_ip", "172.16.0.7", "host_ip", "10.0.0.2"),
			// worker: Unknown phase (노드 유실) → 상태 미상이라 사유 생략 (node-map 과 공용 조건).
			sample(1, "namespace", "app", "pod", "lost", "uid", "u6", "node", "worker", "pod_ip", "172.16.0.8", "host_ip", "10.0.0.2")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u1", "phase", "Running"),
			sample(1, "uid", "u2", "phase", "Running"),
			sample(1, "uid", "u3", "phase", "Running"),
			sample(1, "uid", "u4", "phase", "Running"),
			sample(1, "uid", "u5", "phase", "Succeeded"),
			sample(1, "uid", "u6", "phase", "Unknown")).
		on("netobs_pod_bytes_total", sample(3, "src_namespace", "app", "src_pod", "seen")).
		on("netobs_bpf_program_loaded", sample(26, "node", "worker"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPods(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil))
	var resp PodsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{"apiserver": "agent_absent", "cilium-x": "host_network", "silent": "no_data", "seen": "", "done": "", "lost": ""}
	for _, p := range resp.Pods {
		if w, ok := want[p.Pod]; ok && p.UnobservedReason != w {
			t.Errorf("%s reason=%q want %q", p.Pod, p.UnobservedReason, w)
		}
	}
}

// TestOverview_UnobservablePodsExcluded 는 #320 의 구조적 미관측 분리를 검증한다. agent 미배치와
// hostNetwork 는 unobservable 로 세고 no_data 는 관측 가능 pod 기준으로 남는다.
func TestOverview_UnobservablePodsExcluded(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info",
			sample(1, "namespace", "ks", "pod", "apiserver", "node", "master", "pod_ip", "10.0.0.1", "host_ip", "10.0.0.1"),
			sample(1, "namespace", "ks", "pod", "cilium-x", "node", "worker", "pod_ip", "10.0.0.2", "host_ip", "10.0.0.2"),
			sample(1, "namespace", "app", "pod", "silent", "node", "worker", "pod_ip", "172.16.0.5", "host_ip", "10.0.0.2"),
			sample(1, "namespace", "app", "pod", "seen", "node", "worker", "pod_ip", "172.16.0.6", "host_ip", "10.0.0.2")).
		on("netobs_pod_bytes_total", sample(3, "src_namespace", "app", "src_pod", "seen")).
		on("netobs_bpf_program_loaded", sample(26, "node", "worker")).
		on("kube_pod_status_phase",
			sample(1, "namespace", "ks", "pod", "apiserver", "phase", "Running"),
			sample(1, "namespace", "ks", "pod", "cilium-x", "phase", "Running"),
			sample(1, "namespace", "app", "pod", "silent", "phase", "Running"),
			sample(1, "namespace", "app", "pod", "seen", "phase", "Running"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetOverview(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	var resp OverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Pods.Total != 4 || resp.Pods.Live != 1 || resp.Pods.Unobservable != 2 || resp.Pods.NoData != 1 {
		t.Errorf("pods=%+v want total 4 / live 1 / unobservable 2 / no_data 1", resp.Pods)
	}
}

// TestPods_NoTrafficReason 은 #342 의 no_traffic 세분화를 검증한다. agent 가 배치된 노드에서
// netns 무소켓이 증명된 pod (netobs_pod_no_sockets 시리즈 존재) 는 no_data 대신 no_traffic 으로
// 분류되고, 무소켓 증명이 없는 침묵 pod 는 기존 no_data 를 유지한다. agent 미배치 노드는 소켓
// 시리즈가 있어도 agent_absent 가 우선한다.
func TestPods_NoTrafficReason(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_pod_info",
			sample(1, "namespace", "nvdp", "pod", "device-plugin", "uid", "u-1", "node", "worker", "pod_ip", "10.0.0.1", "host_ip", "192.168.1.2"),
			sample(1, "namespace", "app", "pod", "silent", "uid", "u-2", "node", "worker", "pod_ip", "10.0.0.2", "host_ip", "192.168.1.2"),
			sample(1, "namespace", "app", "pod", "orphan", "uid", "u-3", "node", "master", "pod_ip", "10.0.0.3", "host_ip", "192.168.1.1")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u-1", "phase", "Running"),
			sample(1, "uid", "u-2", "phase", "Running"),
			sample(1, "uid", "u-3", "phase", "Running")).
		on("netobs_pod_no_sockets",
			sample(1, "src_namespace", "nvdp", "src_pod", "device-plugin"),
			sample(1, "src_namespace", "app", "src_pod", "orphan")).
		on("netobs_bpf_program_loaded", sample(26, "node", "worker"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPods(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil))
	var resp PodsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{"device-plugin": "no_traffic", "silent": "no_data", "orphan": "agent_absent"}
	for _, p := range resp.Pods {
		if w, ok := want[p.Pod]; ok && p.UnobservedReason != w {
			t.Errorf("%s reason=%q want %q", p.Pod, p.UnobservedReason, w)
		}
	}
}
