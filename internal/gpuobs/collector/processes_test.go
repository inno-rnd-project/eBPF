package collector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"netobs/internal/gpuobs/config"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// TestProcessSnapshotAndHandler 는 pollOnce 가 프로세스 스냅샷을 축적하고 /processes 핸들러가
// 그것을 JSON 계약 (types.GPUProcessListing) 으로 돌려주는지 검증한다. pod 미귀속 PID 도 목록에
// 포함되고 (namespace/pod 만 공백), SM util 은 수집된 PID 에만 실린다.
func TestProcessSnapshotAndHandler(t *testing.T) {
	devInfo := types.GPUDevice{Index: 0, UUID: "u0"}
	dev := &fakeDevice{
		info:     devInfo,
		snapshot: types.GPUSnapshot{Device: devInfo},
		processes: []types.GPUProcess{
			{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1024, Type: "compute"},
			{DeviceIndex: 0, PID: 200, MemoryUsedBytes: 2048, Type: "graphics"},
		},
		procUtils: []types.GPUProcessUtil{{PID: 100, SmUtilPct: 30}},
	}
	resolver := &fakeResolver{byPID: map[uint32]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "train-a", PodUID: "uid1"},
	}}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "gpu"}
	c, _ := newCollectorWithDevs(t, cfg, resolver, dev)

	c.pollOnce()

	rec := httptest.NewRecorder()
	c.ProcessesHandler()(rec, httptest.NewRequest(http.MethodGet, "/processes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var listing types.GPUProcessListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if listing.Node != "gpu" || listing.CollectedAt == "" {
		t.Errorf("listing=%+v want node gpu / collected_at 채움", listing)
	}
	if len(listing.Processes) != 2 {
		t.Fatalf("processes=%d want 2 (pod 미귀속 포함): %+v", len(listing.Processes), listing.Processes)
	}
	p1, p2 := listing.Processes[0], listing.Processes[1]
	if p1.PID != 100 || p1.Namespace != "ml" || p1.Pod != "train-a" || p1.Type != "compute" || p1.GpuUUID != "u0" {
		t.Errorf("p1=%+v want 100/ml/train-a/compute/u0", p1)
	}
	if p1.SmUtilPercent == nil || *p1.SmUtilPercent != 30 {
		t.Errorf("p1 sm_util=%v want 30", p1.SmUtilPercent)
	}
	if p2.PID != 200 || p2.Namespace != "" || p2.Pod != "" || p2.SmUtilPercent != nil {
		t.Errorf("p2=%+v want 미귀속 (namespace/pod 공백, sm_util 생략)", p2)
	}
	if p2.MemoryUsedBytes != 2048 || p2.Type != "graphics" {
		t.Errorf("p2=%+v want mem 2048/graphics", p2)
	}
}

// TestProcessSnapshot_ReplacedEachPoll 은 다음 poll 에서 종료된 프로세스가 스냅샷에 남지 않는지
// 검증한다 (통째 교체 invariant).
func TestProcessSnapshot_ReplacedEachPoll(t *testing.T) {
	devInfo := types.GPUDevice{Index: 0, UUID: "u0"}
	dev := &fakeDevice{
		info:      devInfo,
		snapshot:  types.GPUSnapshot{Device: devInfo},
		processes: []types.GPUProcess{{DeviceIndex: 0, PID: 100, MemoryUsedBytes: 1, Type: "compute"}},
	}
	cfg := config.Config{GPUMetricsEnabled: true, PodMetricsEnabled: true, NodeName: "gpu"}
	c, _ := newCollectorWithDevs(t, cfg, &fakeResolver{}, dev)

	c.pollOnce()
	if procs, _ := c.ProcessSnapshot(); len(procs) != 1 {
		t.Fatalf("첫 poll procs=%d want 1", len(procs))
	}

	dev.mu.Lock()
	dev.processes = nil
	dev.mu.Unlock()
	c.pollOnce()
	if procs, _ := c.ProcessSnapshot(); len(procs) != 0 {
		t.Errorf("종료 후 procs=%+v want empty (통째 교체)", procs)
	}
}

// TestProcessesHandler_NoSweep 은 스냅샷이 아직 없을 때 (per-pod 비활성 등) 빈 목록과 collected_at
// 공백으로 graceful 응답하는지 검증한다.
func TestProcessesHandler_NoSweep(t *testing.T) {
	c := New(nil, config.Config{NodeName: "worker1"}, nil)
	rec := httptest.NewRecorder()
	c.ProcessesHandler()(rec, httptest.NewRequest(http.MethodGet, "/processes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var listing types.GPUProcessListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listing.Processes) != 0 || listing.CollectedAt != "" {
		t.Errorf("listing=%+v want 빈 목록 / collected_at 공백", listing)
	}
}
