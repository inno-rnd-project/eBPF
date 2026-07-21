package api

import (
	"encoding/json"
	"math"
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
		// +Inf 샘플은 JSON 직렬화가 불가하므로 필터로 배제되어야 한다. 배제가 깨지면 180 을 Inf 가
		// 덮어써 응답 인코딩이 실패한다.
		on("gpuobs_device_power_usage_watts", sample(180, dev...), sample(math.Inf(1), dev...)).
		on("gpuobs_device_power_limit_watts", sample(350, dev...)).
		on("gpuobs_device_temperature_celsius", sample(61, dev...)).
		on("gpuobs_device_throttle_active", sample(1, "node", "gpu", "gpu_uuid", "u1", "reason", "sw_power_cap")).
		on("gpuobs_pod_utilization_percent",
			sample(30, "node", "gpu", "gpu_uuid", "u1", "src_namespace", "ml", "src_pod", "train-a"),
			sample(12, "node", "gpu", "gpu_uuid", "u1", "src_namespace", "ml", "src_pod", "infer-b")).
		on("gpuobs_pod_memory_used_bytes",
			sample(4e9, "node", "gpu", "gpu_uuid", "u1", "src_namespace", "ml", "src_pod", "train-a")).
		// #267 device 상세 확장. 단일값과 서브라벨(clock/threshold), reason 합산(throttle_violation).
		on("gpuobs_device_fan_speed_percent", sample(45, dev...)).
		on("gpuobs_device_performance_state", sample(2, dev...)).
		on("gpuobs_device_pcie_link_generation_current", sample(4, dev...)).
		on("gpuobs_device_pcie_link_width_current", sample(16, dev...)).
		on("gpuobs_device_clock_mhz",
			sample(1800, "node", "gpu", "gpu_uuid", "u1", "clock", "sm"),
			sample(9500, "node", "gpu", "gpu_uuid", "u1", "clock", "mem")).
		on("gpuobs_device_temperature_threshold_celsius",
			sample(83, "node", "gpu", "gpu_uuid", "u1", "threshold", "slowdown"),
			sample(90, "node", "gpu", "gpu_uuid", "u1", "threshold", "shutdown")).
		on("gpuobs_device_throttle_violation_seconds_total",
			sample(4, "node", "gpu", "gpu_uuid", "u1")).
		// #279: driver 심볼 부착, runtime 심볼 미부착 (dev 실측 형태).
		on("gpuobs_cuda_symbol_available",
			sample(1, "node", "gpu", "symbol", "cuLaunchKernel"),
			sample(1, "node", "gpu", "symbol", "cuMemcpy"),
			sample(0, "node", "gpu", "symbol", "cudaLaunchKernel")).
		on("gpuobs_nvml_errors_total", sample(0, "node", "gpu")).
		// #304 노드 dominant cause (유휴 게이팅 충족 노드만 시리즈 존재).
		on("node:gpu_idle_dominant_cause:5m", sample(1.000005, "node", "gpu", "cause", "memory_pressure"))
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
	// #267 device 상세 확장. 단일값 필드.
	if d.FanSpeedPercent == nil || *d.FanSpeedPercent != 45 {
		t.Errorf("fan_speed=%v want 45", d.FanSpeedPercent)
	}
	if d.PcieLinkGeneration == nil || *d.PcieLinkGeneration != 4 || d.PcieLinkWidth == nil || *d.PcieLinkWidth != 16 {
		t.Errorf("pcie=%v/%v want gen 4/width 16", d.PcieLinkGeneration, d.PcieLinkWidth)
	}
	if d.ThrottleViolationSeconds == nil || *d.ThrottleViolationSeconds != 4 {
		t.Errorf("throttle_violation=%v want 4 (reason 합)", d.ThrottleViolationSeconds)
	}
	// 서브라벨 map (clock, threshold).
	if d.ClocksMhz["sm"] != 1800 || d.ClocksMhz["mem"] != 9500 {
		t.Errorf("clocks_mhz=%v want sm 1800/mem 9500", d.ClocksMhz)
	}
	if d.TemperatureThresholdsCelsius["slowdown"] != 83 || d.TemperatureThresholdsCelsius["shutdown"] != 90 {
		t.Errorf("temperature_thresholds=%v want slowdown 83/shutdown 90", d.TemperatureThresholdsCelsius)
	}
	// SM active 는 fixture 에 gpm 시리즈가 없어 (consumer GPU 재현) 생략돼야 한다.
	if d.SMActivePercent != nil {
		t.Errorf("sm_active_percent=%v want nil (GPM 미지원 재현)", d.SMActivePercent)
	}
	// #279 device 상태: sw_power_cap 은 성능성 throttle 이라 degraded.
	if d.Status != "degraded" {
		t.Errorf("status=%q want degraded (sw_power_cap 활성)", d.Status)
	}
	// #304 idle 판정: 사용률 42 는 임계 20 이상이라 idle false.
	if d.Idle {
		t.Errorf("idle=%v want false (사용률 42)", d.Idle)
	}
	// #304 노드 dominant cause 요약: cause 와 카탈로그 description.
	dc, ok := resp.DominantCauses["gpu"]
	if !ok || dc.Cause != "memory_pressure" || dc.Description == "" {
		t.Errorf("dominant_causes=%+v want gpu → memory_pressure + 설명", resp.DominantCauses)
	}
	// #279 귀속 능력: driver 심볼 부착 + runtime 미부착 → available true, runtime false, 사유 명시.
	if d.PodAttribution == nil || !d.PodAttribution.Available || d.PodAttribution.RuntimeSymbols {
		t.Fatalf("pod_attribution=%+v want available/runtime false", d.PodAttribution)
	}
	if d.PodAttribution.Reason == "" {
		t.Errorf("pod_attribution reason 비어 있음 (runtime 미부착 사유 필요)")
	}
}

// TestGpuStatus_DeviceStatus 는 #279 의 device 상태 3단과 driver 심볼 미부착 판정을 검증한다.
func TestGpuStatus_DeviceStatus(t *testing.T) {
	dev := []string{"node", "gpu", "gpu_uuid", "u1", "gpu_index", "0"}
	q := (&fakeQuerier{}).
		on("gpuobs_device_utilization_percent", sample(5, dev...)).
		on("gpuobs_device_temperature_celsius", sample(86, dev...)).
		on("gpuobs_device_temperature_threshold_celsius",
			sample(93, "node", "gpu", "gpu_uuid", "u1", "threshold", "slowdown")).
		// 정보성 gpu_idle 만 활성 → degraded 아님. 온도 86 >= 0.9*93(83.7) → warning.
		on("gpuobs_device_throttle_active", sample(1, "node", "gpu", "gpu_uuid", "u1", "reason", "gpu_idle")).
		// driver 심볼 미부착 → 귀속 불가.
		on("gpuobs_cuda_symbol_available", sample(0, "node", "gpu", "symbol", "cuLaunchKernel"))
	h := NewSynthesisHandler(q, nil, nil)
	rec := httptest.NewRecorder()
	h.GetGpuStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/gpu-status", nil))
	var resp GpuStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := resp.Devices[0]
	// #304 사용률 5 는 임계 20 미만이라 idle true. dominant cause 시리즈 미등록 노드는 맵 생략.
	if !d.Idle {
		t.Errorf("idle=%v want true (사용률 5)", d.Idle)
	}
	if resp.DominantCauses != nil {
		t.Errorf("dominant_causes=%+v want 생략 (시리즈 부재)", resp.DominantCauses)
	}
	if d.Status != "warning" {
		t.Errorf("status=%q want warning (slowdown 임계 90%% 근접, gpu_idle 은 정보성)", d.Status)
	}
	if d.PodAttribution == nil || d.PodAttribution.Available {
		t.Errorf("pod_attribution=%+v want available false (driver 심볼 미부착)", d.PodAttribution)
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
