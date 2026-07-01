package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func memoryFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("container_memory_working_set_bytes",
			sample(900e6, "node", "gpu", "namespace", "default", "pod", "pod-a"),
			sample(100e6, "node", "gpu", "namespace", "default", "pod", "pod-b")).
		on("container_memory_rss",
			sample(850e6, "node", "gpu", "namespace", "default", "pod", "pod-a"),
			sample(20e6, "node", "gpu", "namespace", "default", "pod", "pod-b")).
		on("container_memory_cache",
			sample(50e6, "node", "gpu", "namespace", "default", "pod", "pod-a"),
			sample(80e6, "node", "gpu", "namespace", "default", "pod", "pod-b")).
		on("container_memory_swap").
		on("kube_pod_container_resource_limits", sample(1000e6, "namespace", "default", "pod", "pod-a"))
}

// TestMemory 는 /api/v1/memory 가 pod별 종류별 메모리 분해와 OOM 위험, 지배 종류를 합성하는지 검증한다.
// pod-a 는 limit 이 있어 rss 지배 + 90% OOM 위험, pod-b 는 limit 미설정 + cache 지배다.
func TestMemory(t *testing.T) {
	h := NewSynthesisHandler(memoryFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetMemory(rec, httptest.NewRequest(http.MethodGet, "/api/v1/memory", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp MemoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pods) != 2 || resp.Pods[0].Pod != "pod-a" {
		t.Fatalf("pods=%+v want pod-a 먼저(OOM 위험)", resp.Pods)
	}
	a := resp.Pods[0]
	if a.OOMRisk == nil || *a.OOMRisk < 0.89 || *a.OOMRisk > 0.91 || a.Severity != "high" || a.DominantKind != "rss" {
		t.Errorf("pod-a=%+v want oom 0.9/high/rss dominant", a)
	}
	if a.RSSBytes != 850e6 || a.CacheBytes != 50e6 {
		t.Errorf("pod-a bytes=%+v want rss 850M/cache 50M", a)
	}
	b := resp.Pods[1]
	if b.OOMRisk != nil || b.Severity != "unknown" || b.DominantKind != "cache" {
		t.Errorf("pod-b=%+v want limit 없음(oom nil/unknown), cache dominant", b)
	}
}

// TestMemory_NamespaceFilter 는 ?namespace 가 PromQL label matcher 로 쿼리에 들어가는지 검증한다.
func TestMemory_NamespaceFilter(t *testing.T) {
	q := memoryFakeQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetMemory(rec, httptest.NewRequest(http.MethodGet, "/api/v1/memory?namespace=default", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !q.sawQuery(`namespace="default"`) {
		t.Errorf("실행된 쿼리에 namespace 셀렉터 없음")
	}
}

// TestMemory_QueryError 는 주 소스(working_set) 쿼리 실패 시 500 을 돌려주는지 검증한다.
func TestMemory_QueryError(t *testing.T) {
	h := NewSynthesisHandler(errQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetMemory(rec, httptest.NewRequest(http.MethodGet, "/api/v1/memory", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500 (working_set query 실패)", rec.Code)
	}
}

// TestMemory_NilQuerier 는 querier 가 nil 일 때 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestMemory_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetMemory(rec, httptest.NewRequest(http.MethodGet, "/api/v1/memory", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp MemoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pods) != 0 {
		t.Errorf("pods=%d want 0 (nil querier)", len(resp.Pods))
	}
}
