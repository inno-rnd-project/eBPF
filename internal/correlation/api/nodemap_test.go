package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func nodeMapFakeQuerier() *fakeQuerier {
	return (&fakeQuerier{}).
		on("kube_node_info",
			sample(1, "node", "gpu"),
			sample(1, "node", "worker1")).
		on("kube_node_status_condition",
			sample(1, "node", "gpu", "condition", "Ready", "status", "true"),
			sample(1, "node", "worker1", "condition", "Ready", "status", "true")).
		on("kube_node_role", sample(1, "node", "worker1", "role", "control-plane")).
		on("kube_node_status_capacity", sample(2, "node", "gpu", "resource", "nvidia_com_gpu")).
		on("node:pressure_score:5m", sample(0.1, "node", "gpu"), sample(0.1, "node", "worker1")).
		// trainer 를 가리키는 firing alert 2종 (src_pod 와 victim_pod 규약). gpu 노드는 node 라벨로 경고.
		on("ALERTS",
			sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "node", "gpu", "src_pod", "trainer"),
			sample(1, "alertname", "CorrelationStrongNoisyNeighbor", "severity", "warning", "victim_pod", "trainer")).
		on("kube_pod_info",
			sample(1, "namespace", "ns1", "pod", "trainer", "uid", "u1", "node", "gpu"),
			sample(1, "namespace", "ns1", "pod", "crashed", "uid", "u2", "node", "gpu"),
			sample(1, "namespace", "ns2", "pod", "web", "uid", "u3", "node", "worker1"),
			sample(1, "namespace", "ns1", "pod", "job-done", "uid", "u4", "node", "gpu")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u1", "phase", "Running"),
			sample(1, "uid", "u2", "phase", "Failed"),
			sample(1, "uid", "u3", "phase", "Running"),
			sample(1, "uid", "u4", "phase", "Succeeded")).
		on("netobs_pod_bytes_total", sample(3, "src_namespace", "ns1", "src_pod", "trainer")).
		// #320 사유 판별: gpu 노드만 netobs agent 배치.
		on("netobs_bpf_program_loaded", sample(26, "node", "gpu"))
}

// TestNodeMap 은 노드 그리드 합성 (pod 수 내림차순 정렬, GPU 뱃지, 노드/pod 3단 상태, alert 매칭
// issues, no-data) 을 검증한다.
func TestNodeMap(t *testing.T) {
	h := NewSynthesisHandler(nodeMapFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp NodeMapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 2 || resp.Nodes[0].Name != "gpu" {
		t.Fatalf("nodes=%+v want gpu (pod 3) 선두", resp.Nodes)
	}
	gpu := resp.Nodes[0]
	if !gpu.GPU || gpu.GPUDevices != 2 || gpu.Status != "warning" || gpu.PodCount != 3 {
		t.Errorf("gpu 노드=%+v want gpu/devices2/warning/pod3", gpu)
	}
	// pod 정렬은 namespace·pod 사전순: crashed, job-done, trainer.
	if gpu.Pods[0].Pod != "crashed" || gpu.Pods[0].Status != "down" {
		t.Errorf("pods[0]=%+v want crashed down (phase Failed)", gpu.Pods[0])
	}
	// #314 정상 종료 Job pod 는 completed 로 분리된다 (telemetry 부재가 정상이라 live 오독 방지).
	// #320 종료 pod 는 no-data 사유가 생략된다.
	if gpu.Pods[1].Pod != "job-done" || gpu.Pods[1].Status != "completed" || gpu.Pods[1].NoDataReason != "" {
		t.Errorf("pods[1]=%+v want job-done completed (사유 생략)", gpu.Pods[1])
	}
	trainer := gpu.Pods[2]
	if trainer.Status != "warning" || trainer.NoData {
		t.Errorf("trainer=%+v want warning (alert 매칭) + 관측 중", trainer)
	}
	// issues: 규약 2종 (src_pod, victim_pod) 매칭 alertname 이 사전순 dedup 으로 들어간다.
	if len(trainer.Issues) != 2 || trainer.Issues[0] != "CorrelationStrongNoisyNeighbor" || trainer.Issues[1] != "NetObsDropBurst" {
		t.Errorf("issues=%v want 2종 사전순", trainer.Issues)
	}
	worker := resp.Nodes[1]
	if worker.Status != "healthy" || len(worker.Roles) != 1 || worker.Pods[0].Status != "live" || !worker.Pods[0].NoData {
		t.Errorf("worker=%+v want healthy/roles1/live/no-data", worker)
	}
	// #320 worker1 은 agent 미배치라 no-data 사유가 agent_absent 다.
	if worker.Pods[0].NoDataReason != "agent_absent" {
		t.Errorf("worker no_data_reason=%q want agent_absent", worker.Pods[0].NoDataReason)
	}
}

// TestNodeMap_NodeFilter 는 단일 노드 조회와 미등록 노드 404 를 검증한다.
func TestNodeMap_NodeFilter(t *testing.T) {
	h := NewSynthesisHandler(nodeMapFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?node=worker1", nil))
	var resp NodeMapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Name != "worker1" {
		t.Fatalf("nodes=%+v want worker1 단건", resp.Nodes)
	}
	rec = httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?node=bogus", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404 (미등록 노드)", rec.Code)
	}
}

// TestNodeMap_CrossNamespaceAlert 는 #252 의 회귀 케이스다. 동일 이름 pod 가 두 namespace 에 있을
// 때 한 namespace 를 가리키는 alert (src_namespace 쌍) 가 다른 namespace 의 동명 pod 에 붙지 않아야
// 한다.
func TestNodeMap_CrossNamespaceAlert(t *testing.T) {
	q := (&fakeQuerier{}).
		on("kube_node_info", sample(1, "node", "n1")).
		on("kube_node_status_condition", sample(1, "node", "n1", "condition", "Ready", "status", "true")).
		on("ALERTS", sample(1, "alertname", "NetObsDropBurst", "severity", "critical", "src_namespace", "ns-a", "src_pod", "sidecar")).
		on("kube_pod_info",
			sample(1, "namespace", "ns-a", "pod", "sidecar", "uid", "u1", "node", "n1"),
			sample(1, "namespace", "ns-b", "pod", "sidecar", "uid", "u2", "node", "n1")).
		on("kube_pod_status_phase",
			sample(1, "uid", "u1", "phase", "Running"),
			sample(1, "uid", "u2", "phase", "Running"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map", nil))
	var resp NodeMapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byNS := map[string]NodeMapPod{}
	for _, p := range resp.Nodes[0].Pods {
		byNS[p.Namespace] = p
	}
	if a := byNS["ns-a"]; a.Status != "warning" || len(a.Issues) != 1 {
		t.Errorf("ns-a/sidecar=%+v want warning + issues 1건 (alert 귀속)", a)
	}
	if b := byNS["ns-b"]; b.Status != "live" || len(b.Issues) != 0 {
		t.Errorf("ns-b/sidecar=%+v want live + issues 없음 (cross-namespace 오탐 금지)", b)
	}
}

// TestNodeMap_AtParam 은 at 전파와 잘못된 at 의 400 을 검증한다.
func TestNodeMap_AtParam(t *testing.T) {
	q := nodeMapFakeQuerier()
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?at=1751943600", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if q.lastAt.Unix() != 1751943600 {
		t.Errorf("lastAt=%v want unix 1751943600 전파", q.lastAt)
	}
	rec = httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?at=bogus", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400", rec.Code)
	}
}

// TestNodeMap_NodeFilterPushdown 은 #411 의 필터 하강을 고정한다. node 파라미터가 있으면 node 라벨을
// 가진 쿼리에 exact 매처가 실려 Prometheus 단에서 좁혀지고, 라벨 규약이 다른 ALERTS 와 축이 다른
// netobs pod 계열은 하강 대상이 아니다.
func TestNodeMap_NodeFilterPushdown(t *testing.T) {
	// 노드가 존재해야 200 이다 (미등록 노드는 404 로 종전 계약 유지).
	q := (&fakeQuerier{}).on("kube_node_info", sample(1, "node", "gpu"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetNodeMap(rec, httptest.NewRequest(http.MethodGet, "/api/v1/node-map?node=gpu", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	for _, want := range []string{
		`kube_node_info{node="gpu"}`,
		`kube_pod_info{node="gpu"}`,
		`node:pressure_score:5m{node="gpu"}`,
	} {
		if !q.sawQuery(want) {
			t.Errorf("하강 쿼리 %q 미확인: %v", want, q.queries)
		}
	}
	// ALERTS 는 노드 라벨 규약이 alert 별로 달라 하강하지 않는다.
	if q.sawQuery(`ALERTS{alertstate="firing",node=`) {
		t.Errorf("ALERTS 에 node 매처가 결합됨: %v", q.queries)
	}
	// 필터 미지정이면 종전과 동일한 전 클러스터 쿼리다.
	q2 := (&fakeQuerier{}).on("kube_node_info", sample(1, "node", "gpu"))
	h2 := NewSynthesisHandler(q2, nil, nil)
	h2.GetNodeMap(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/node-map", nil))
	if !q2.sawQuery("kube_node_info") || q2.sawQuery(`kube_node_info{node=`) {
		t.Errorf("미지정 시 bare 쿼리가 아님: %v", q2.queries)
	}
}
