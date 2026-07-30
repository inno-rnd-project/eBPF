package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #383 pod 식별자 표현 정합 테스트다. 결합 표현 (ns/pod) 을 쓰던 세 API (/health hotspot·dominant,
// /pressure scope=pod, node/{node} top_pods) 가 분리 필드 (namespace 와 pod_name 계열) 를 additive
// 로 병기하고, 결합 필드가 분리 필드의 재구성과 항상 일치하는지 본다. namespace 미상이면 결합
// 표현은 _unknown sentinel, 분리 필드는 생략이다.

// combinedFrom 은 분리 필드에서 결합 id 표현을 재구성한다 (podLabel 규약의 역산).
func combinedFrom(namespace, name string) string {
	if name == "" {
		return ""
	}
	if namespace == "" {
		namespace = "_unknown"
	}
	return namespace + "/" + name
}

func TestPodIdentity_HealthHotspotAndDominant(t *testing.T) {
	q := (&fakeQuerier{}).
		on("node:cpu_pressure_score", sample(0.82, "node", "worker1")).
		on("pod:cpu_throttle_score", sample(0.78, "node", "worker1", "src_namespace", "default", "src_pod", "app-x"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetHealth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	var resp HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hs := resp.Dimensions["cpu"].Hotspot
	if hs == nil || hs.TopPod != "default/app-x" || hs.TopPodNamespace != "default" || hs.TopPodName != "app-x" {
		t.Errorf("hotspot=%+v want top_pod default/app-x + 분리 필드 default/app-x", hs)
	}
	if hs != nil && hs.TopPod != combinedFrom(hs.TopPodNamespace, hs.TopPodName) {
		t.Errorf("top_pod %q 가 분리 필드 재구성 %q 과 불일치", hs.TopPod, combinedFrom(hs.TopPodNamespace, hs.TopPodName))
	}
	d := resp.DominantPressure
	if d == nil || d.Pod != "default/app-x" || d.Namespace != "default" || d.PodName != "app-x" {
		t.Errorf("dominant=%+v want pod default/app-x + 분리 필드 default/app-x", d)
	}
}

func TestPodIdentity_PressureRanking(t *testing.T) {
	q := (&fakeQuerier{}).
		on("pod:cpu_throttle_score",
			sample(0.9, "node", "worker1", "src_namespace", "default", "src_pod", "app-x"),
			sample(0.5, "node", "worker2", "src_pod", "orphan"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetPressure(rec, httptest.NewRequest(http.MethodGet, "/api/v1/pressure?dimension=cpu&scope=pod", nil))
	var resp PressureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Ranking) != 2 {
		t.Fatalf("ranking=%d want 2", len(resp.Ranking))
	}
	top := resp.Ranking[0]
	if top.Pod != "default/app-x" || top.Namespace != "default" || top.PodName != "app-x" {
		t.Errorf("rank1=%+v want default/app-x 분리 병기", top)
	}
	// namespace 미상: 결합 표현은 _unknown sentinel, 분리 필드는 namespace 생략 + pod_name 유지.
	orphan := resp.Ranking[1]
	if orphan.Pod != "_unknown/orphan" || orphan.Namespace != "" || orphan.PodName != "orphan" {
		t.Errorf("rank2=%+v want _unknown/orphan + namespace 생략 + pod_name orphan", orphan)
	}
	for _, e := range resp.Ranking {
		if e.Pod != combinedFrom(e.Namespace, e.PodName) {
			t.Errorf("pod %q 가 분리 필드 재구성 %q 과 불일치", e.Pod, combinedFrom(e.Namespace, e.PodName))
		}
	}
}

func TestPodIdentity_NodeTopPods(t *testing.T) {
	q := (&fakeQuerier{}).
		on("pod:cpu_throttle_score", sample(0.78, "node", "worker2", "src_namespace", "default", "src_pod", "app-x")).
		on("pod:memory_pressure_score", sample(0.5, "node", "worker2", "src_pod", "orphan"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNode(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node/worker2", nil))
	var resp NodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.TopPods) != 2 {
		t.Fatalf("top_pods=%+v want 2건", resp.TopPods)
	}
	if resp.TopPods[0].Pod != "default/app-x" || resp.TopPods[0].Namespace != "default" || resp.TopPods[0].PodName != "app-x" {
		t.Errorf("top1=%+v want default/app-x 분리 병기", resp.TopPods[0])
	}
	if resp.TopPods[1].Pod != "_unknown/orphan" || resp.TopPods[1].Namespace != "" || resp.TopPods[1].PodName != "orphan" {
		t.Errorf("top2=%+v want _unknown/orphan + namespace 생략", resp.TopPods[1])
	}
	for _, p := range resp.TopPods {
		if p.Pod != combinedFrom(p.Namespace, p.PodName) {
			t.Errorf("pod %q 가 분리 필드 재구성 %q 과 불일치", p.Pod, combinedFrom(p.Namespace, p.PodName))
		}
	}
}
