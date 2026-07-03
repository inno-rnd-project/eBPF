package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func gpuStatusFakeQuerier() *fakeQuerier {
	dev := []string{"node", "gpu", "gpu_uuid", "u1", "gpu_index", "0", "gpu_model", "RTX 3090"}
	return (&fakeQuerier{}).
		on("gpuobs_device_utilization_percent", sample(42, dev...)).
		on("gpuobs_device_memory_used_bytes", sample(6e9, dev...)).
		on("gpuobs_device_memory_total_bytes", sample(24e9, dev...)).
		on("gpuobs_device_power_usage_watts", sample(180, dev...)).
		on("gpuobs_device_power_limit_watts", sample(350, dev...)).
		on("gpuobs_device_temperature_celsius", sample(61, dev...)).
		on("gpuobs_device_throttle_active", sample(1, "node", "gpu", "gpu_uuid", "u1", "reason", "sw_power_cap")).
		on("gpuobs_pod_utilization_percent",
			sample(30, "node", "gpu", "gpu_uuid", "u1", "src_namespace", "ml", "src_pod", "train-a"),
			sample(12, "node", "gpu", "gpu_uuid", "u1", "src_namespace", "ml", "src_pod", "infer-b")).
		on("gpuobs_pod_memory_used_bytes",
			sample(4e9, "node", "gpu", "gpu_uuid", "u1", "src_namespace", "ml", "src_pod", "train-a"))
}

// TestGpuStatus 는 device 현황 신호 병합과 memory ratio 산출, 활성 throttle reason 수집, pod 점유
// 병합 (utilization 내림차순 정렬) 을 검증한다.
func TestGpuStatus(t *testing.T) {
	h := NewSynthesisHandler(gpuStatusFakeQuerier(), nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Devices) != 1 {
		t.Fatalf("devices=%d want 1", len(resp.Devices))
	}
	d := resp.Devices[0]
	if d.Node != "gpu" || d.GpuUUID != "u1" || d.Model != "RTX 3090" || d.UtilizationPercent != 42 {
		t.Errorf("device=%+v want gpu/u1/RTX 3090/42", d)
	}
	if d.MemoryUsedRatio == nil || *d.MemoryUsedRatio != 0.25 {
		t.Errorf("memory_used_ratio=%v want 0.25", d.MemoryUsedRatio)
	}
	if d.PowerUsageWatts == nil || *d.PowerUsageWatts != 180 || d.TemperatureCelsius == nil || *d.TemperatureCelsius != 61 {
		t.Errorf("power/temp=%+v want 180/61", d)
	}
	if len(d.ThrottleReasons) != 1 || d.ThrottleReasons[0] != "sw_power_cap" {
		t.Errorf("throttle_reasons=%v want [sw_power_cap]", d.ThrottleReasons)
	}
	if len(d.Pods) != 2 || d.Pods[0].Pod != "train-a" || d.Pods[1].Pod != "infer-b" {
		t.Fatalf("pods=%+v want train-a(30) 먼저", d.Pods)
	}
	if d.Pods[0].MemoryUsedBytes == nil || *d.Pods[0].MemoryUsedBytes != 4e9 {
		t.Errorf("train-a memory=%v want 4e9 (util+memory 병합)", d.Pods[0].MemoryUsedBytes)
	}
	if d.Pods[1].MemoryUsedBytes != nil {
		t.Errorf("infer-b memory=%v want nil (memory 미수집 pod)", d.Pods[1].MemoryUsedBytes)
	}
}

// TestGpuStatus_QueryError 는 주 소스 (utilization) 쿼리 실패 시 500 을 돌려주는지 검증한다.
func TestGpuStatus_QueryError(t *testing.T) {
	h := NewSynthesisHandler(errQuerier{}, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status=%d want 500", rec.Code)
	}
}

// TestGpuStatus_NilQuerier 는 querier 미주입 시 panic 없이 빈 응답을 돌려주는지 검증한다.
func TestGpuStatus_NilQuerier(t *testing.T) {
	h := NewSynthesisHandler(nil, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var resp GpuStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Devices) != 0 {
		t.Errorf("devices=%+v want empty", resp.Devices)
	}
}
