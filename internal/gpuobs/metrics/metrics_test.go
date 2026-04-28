package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// resetPodMetricsState는 패키지 레벨 podMemoryUsed gauge / podMetricsEnabled 토글 / lastPodSampleKeys
// diff 추적기를 테스트마다 초기화해 case 간 라벨 누수가 일어나지 않도록 한다.
func resetPodMetricsState(t *testing.T) {
	t.Helper()
	podMemoryUsed.Reset()
	podMetricsEnabled = true
	lastPodSampleKeys = make(map[string]struct{})
}

// resetDeviceMetricsState는 패키지 레벨 device gauge/counter와 ECC delta 추적기를 초기화한다.
// counter는 Reset() 후 새 series를 만들 때부터 0에서 시작해 Add(delta)가 올바르게 누적된다.
func resetDeviceMetricsState(t *testing.T) {
	t.Helper()
	deviceUtilization.Reset()
	deviceMemoryUsed.Reset()
	deviceMemoryTotal.Reset()
	deviceTemperature.Reset()
	devicePower.Reset()
	deviceMemoryCopyUtilization.Reset()
	devicePcieRxBps.Reset()
	devicePcieTxBps.Reset()
	deviceThrottleActive.Reset()
	deviceClockMhz.Reset()
	deviceEccErrors.Reset()
	deviceEncoderUtilization.Reset()
	deviceDecoderUtilization.Reset()
	devicePerformanceState.Reset()
	lastEccAbsolute = make(map[string]uint64)
}

func samplePod(ns, name, uid string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     ns,
		PodName:       name,
		PodUID:        uid,
	}
}

func TestRecordPodSnapshot_WritesAllLabelsAndValue(t *testing.T) {
	resetPodMetricsState(t)

	samples := []PodGPUSample{
		{
			ID:           samplePod("ml", "trainer-0", "uid-xyz"),
			Device:       types.GPUDevice{Index: 1, UUID: "GPU-uuid-1", Model: "A100"},
			MemUsedBytes: 1234,
		},
	}
	RecordPodSnapshot("node-a", samples)

	got := testutil.ToFloat64(podMemoryUsed.WithLabelValues("node-a", "ml", "trainer-0", "uid-xyz", "GPU-uuid-1", "1"))
	if got != 1234 {
		t.Fatalf("podMemoryUsed=%v want 1234", got)
	}
	if got := testutil.CollectAndCount(podMemoryUsed); got != 1 {
		t.Fatalf("series count=%d want 1", got)
	}
}

func TestRecordPodSnapshot_DisabledSkipsNewWritesButCleansPrevious(t *testing.T) {
	// 직전 poll에 series를 만든 뒤 disable을 켜면, 다음 호출에서 신규 기록은 막히되
	// 직전 series는 cleanup 경로로 제거되어야 한다. 토글 off 직후 stale 잔존을 차단한다.
	resetPodMetricsState(t)

	id := samplePod("ml", "p", "u")
	dev := types.GPUDevice{Index: 0, UUID: "GPU-1"}
	RecordPodSnapshot("n", []PodGPUSample{{ID: id, Device: dev, MemUsedBytes: 100}})
	if got := testutil.CollectAndCount(podMemoryUsed); got != 1 {
		t.Fatalf("setup: series count=%d want 1", got)
	}

	SetPodMetricsEnabled(false)
	t.Cleanup(func() { SetPodMetricsEnabled(true) })

	RecordPodSnapshot("n", []PodGPUSample{{ID: id, Device: dev, MemUsedBytes: 999}})
	if got := testutil.CollectAndCount(podMemoryUsed); got != 0 {
		t.Fatalf("disabled toggle must clean previous series; got %d", got)
	}
}

func TestRecordPodSnapshot_NonPodSamplesSkipped(t *testing.T) {
	resetPodMetricsState(t)

	dev := types.GPUDevice{Index: 0, UUID: "GPU-1"}
	samples := []PodGPUSample{
		{ID: kube.PodIdentity{IdentityClass: kube.IdentityClassUnresolved}, Device: dev, MemUsedBytes: 1},
		{ID: kube.PodIdentity{IdentityClass: kube.IdentityClassNode, NodeName: "n1"}, Device: dev, MemUsedBytes: 1},
		{ID: kube.PodIdentity{IdentityClass: kube.IdentityClassExternal}, Device: dev, MemUsedBytes: 1},
		{ID: kube.PodIdentity{IdentityClass: kube.IdentityClassService}, Device: dev, MemUsedBytes: 1},
		{ID: kube.PodIdentity{}, Device: dev, MemUsedBytes: 1},
	}
	RecordPodSnapshot("n", samples)

	if got := testutil.CollectAndCount(podMemoryUsed); got != 0 {
		t.Fatalf("non-pod identities must not be recorded; series count=%d", got)
	}
}

func TestRecordPodSnapshot_MissingPodNameAndUIDFallback(t *testing.T) {
	// Pod으로 분류되었지만 PodName/PodUID가 비어 있는 비정상 입력에서도 빈 라벨로 기록되지 않아야 한다.
	// "unknown" fallback이 적용되어 카디널리티 안전망 역할을 한다.
	resetPodMetricsState(t)

	id := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ml"}
	dev := types.GPUDevice{Index: 0, UUID: "GPU-uuid-x"}
	RecordPodSnapshot("n", []PodGPUSample{{ID: id, Device: dev, MemUsedBytes: 42}})

	got := testutil.ToFloat64(podMemoryUsed.WithLabelValues("n", "ml", "unknown", "unknown", "GPU-uuid-x", "0"))
	if got != 42 {
		t.Fatalf("podMemoryUsed=%v want 42 (fallback labels)", got)
	}
}

func TestRecordPodSnapshot_DiffCleanupRemovesStaleSeries(t *testing.T) {
	// 직전 poll에 podA와 podB가 있었고, 이번 poll에는 podA만 남으면 podB 시리즈는 제거되어야 한다.
	// Reset 방식이 아닌 surgical Delete로 podA 시리즈는 유지된다.
	resetPodMetricsState(t)

	dev := types.GPUDevice{Index: 0, UUID: "GPU-1"}
	podA := samplePod("ml", "a", "uid-a")
	podB := samplePod("ml", "b", "uid-b")

	RecordPodSnapshot("n", []PodGPUSample{
		{ID: podA, Device: dev, MemUsedBytes: 100},
		{ID: podB, Device: dev, MemUsedBytes: 200},
	})
	if got := testutil.CollectAndCount(podMemoryUsed); got != 2 {
		t.Fatalf("after first snapshot series=%d want 2", got)
	}

	RecordPodSnapshot("n", []PodGPUSample{
		{ID: podA, Device: dev, MemUsedBytes: 150},
	})
	if got := testutil.CollectAndCount(podMemoryUsed); got != 1 {
		t.Fatalf("after diff cleanup series=%d want 1 (only podA)", got)
	}
	gotA := testutil.ToFloat64(podMemoryUsed.WithLabelValues("n", "ml", "a", "uid-a", "GPU-1", "0"))
	if gotA != 150 {
		t.Fatalf("podA value=%v want 150 (updated)", gotA)
	}
}

func TestRecordPodSnapshot_EmptyAfterNonEmptyCleansAll(t *testing.T) {
	// 모든 GPU 워크로드가 종료되어 빈 snapshot이 들어오면 직전 poll의 모든 series가 제거되어야 한다.
	resetPodMetricsState(t)

	dev := types.GPUDevice{Index: 0, UUID: "GPU-1"}
	RecordPodSnapshot("n", []PodGPUSample{
		{ID: samplePod("ml", "a", "uid-a"), Device: dev, MemUsedBytes: 1},
		{ID: samplePod("ml", "b", "uid-b"), Device: dev, MemUsedBytes: 2},
	})
	RecordPodSnapshot("n", nil)

	if got := testutil.CollectAndCount(podMemoryUsed); got != 0 {
		t.Fatalf("empty snapshot must clean everything; series=%d", got)
	}
}

func TestRecordPodSnapshot_SameLabelsOverwriteValue(t *testing.T) {
	// 같은 라벨 셋의 sample이 한 snapshot 안에 중복 등장하면 마지막 값으로 덮인다(이전 poll 라벨 셋과는 무관).
	// collector는 합산 후 단일 sample만 보내는 계약이지만, metrics 자체 계약을 명확히 하기 위해 검증한다.
	resetPodMetricsState(t)

	id := samplePod("ml", "a", "uid-a")
	dev := types.GPUDevice{Index: 0, UUID: "GPU-1"}
	RecordPodSnapshot("n", []PodGPUSample{
		{ID: id, Device: dev, MemUsedBytes: 100},
		{ID: id, Device: dev, MemUsedBytes: 200},
	})

	got := testutil.ToFloat64(podMemoryUsed.WithLabelValues("n", "ml", "a", "uid-a", "GPU-1", "0"))
	if got != 200 {
		t.Fatalf("same-label duplicate within one snapshot keeps last; got %v want 200", got)
	}
}

// --------------------- device 메트릭 (Phase 4) ---------------------

// fullySupportedSnap은 모든 *Supported 플래그가 true이고 값이 채워진 최소 snapshot을 만든다.
// 개별 case는 이 baseline에서 필요한 값만 덮어쓰는 형태로 사용한다.
func fullySupportedSnap() types.GPUSnapshot {
	return types.GPUSnapshot{
		Device:                    types.GPUDevice{Index: 0, UUID: "GPU-A", Model: "RTX-3090"},
		UtilizationPct:            50,
		MemoryCopyUtilPct:         30,
		MemoryUsedBytes:           1024,
		MemoryTotalBytes:          8192,
		TemperatureC:              60,
		PowerUsageWatts:           120.5,
		PcieRxBps:                 2048,
		PcieTxBps:                 4096,
		PcieSupported:             true,
		ThrottleReasons:           1 | 8, // gpu_idle + hw_slowdown
		ThrottleReasonsSupported:  true,
		ClockSMMhz:                1500,
		ClockSMSupported:          true,
		ClockMemoryMhz:            9750,
		ClockMemorySupported:      true,
		ClockGraphicsMhz:          1200,
		ClockGraphicsSupported:    true,
		EccCorrectedTotal:         5,
		EccUncorrectedTotal:       1,
		EccSupported:              true,
		EncoderUtilPct:            10,
		EncoderSupported:          true,
		DecoderUtilPct:            20,
		DecoderSupported:          true,
		PerformanceState:          0,
		PerformanceStateSupported: true,
	}
}

func TestRecord_BaseDeviceMetrics(t *testing.T) {
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	Record("n", snap)

	if got := testutil.ToFloat64(deviceUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 50 {
		t.Errorf("util=%v want 50", got)
	}
	if got := testutil.ToFloat64(deviceMemoryCopyUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 30 {
		t.Errorf("memcopy util=%v want 30", got)
	}
	if got := testutil.ToFloat64(devicePerformanceState.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 0 {
		t.Errorf("pstate=%v want 0", got)
	}
	if got := testutil.ToFloat64(deviceEncoderUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 10 {
		t.Errorf("encoder util=%v want 10", got)
	}
	if got := testutil.ToFloat64(deviceDecoderUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 20 {
		t.Errorf("decoder util=%v want 20", got)
	}
}

func TestRecord_PciePerSecondNormalization(t *testing.T) {
	// NVML KB/s × 1024 = bytes/s. 본 함수는 nvml 계층에서 변환 후 Bps 필드에 들어온다고 가정하므로,
	// metrics.Record는 들어온 값을 그대로 발행하는지를 검증한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.PcieRxBps = 2048 * 1024
	snap.PcieTxBps = 5120 * 1024
	Record("n", snap)

	if got := testutil.ToFloat64(devicePcieRxBps.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != float64(2048*1024) {
		t.Errorf("pcie rx=%v", got)
	}
	if got := testutil.ToFloat64(devicePcieTxBps.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != float64(5120*1024) {
		t.Errorf("pcie tx=%v", got)
	}
}

func TestRecord_ThrottleReasonsBitmaskDecomposition(t *testing.T) {
	// bitmask = 1 (gpu_idle) | 8 (hw_slowdown) → 두 reason은 1, 나머지 7개는 0이어야 한다.
	// 매 poll마다 9 reason 모두를 명시 발행해 stale 라벨을 회피한다는 계약 검증.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.ThrottleReasons = 1 | 8
	Record("n", snap)

	if got := testutil.ToFloat64(deviceThrottleActive.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "gpu_idle")); got != 1 {
		t.Errorf("gpu_idle=%v want 1", got)
	}
	if got := testutil.ToFloat64(deviceThrottleActive.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "hw_slowdown")); got != 1 {
		t.Errorf("hw_slowdown=%v want 1", got)
	}
	if got := testutil.ToFloat64(deviceThrottleActive.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "sw_power_cap")); got != 0 {
		t.Errorf("sw_power_cap=%v want 0", got)
	}
	if got := testutil.CollectAndCount(deviceThrottleActive); got != len(throttleReasonBits) {
		t.Errorf("throttle series count=%d want %d (all reasons emitted)", got, len(throttleReasonBits))
	}
}

func TestRecord_ClockDomainsLabeled(t *testing.T) {
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	Record("n", snap)

	if got := testutil.ToFloat64(deviceClockMhz.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "sm")); got != 1500 {
		t.Errorf("clock sm=%v want 1500", got)
	}
	if got := testutil.ToFloat64(deviceClockMhz.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "memory")); got != 9750 {
		t.Errorf("clock memory=%v want 9750", got)
	}
	if got := testutil.ToFloat64(deviceClockMhz.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "graphics")); got != 1200 {
		t.Errorf("clock graphics=%v want 1200", got)
	}
	if got := testutil.CollectAndCount(deviceClockMhz); got != 3 {
		t.Errorf("clock series count=%d want 3 domains", got)
	}
}

func TestRecord_PerDomainSupportedFlagSkipsMetric(t *testing.T) {
	// SM clock만 미지원, Memory/Graphics는 지원되는 경우 sm 라벨만 발행되지 않아야 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.ClockSMSupported = false
	Record("n", snap)

	if got := testutil.CollectAndCount(deviceClockMhz); got != 2 {
		t.Errorf("clock series=%d want 2 (sm skipped)", got)
	}
}

func TestRecord_EccDeltaTracking(t *testing.T) {
	// 첫 poll: prev 없음 → current 그대로 delta로 적용. 두 번째 poll: 증가분만 적용.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.EccCorrectedTotal = 10
	snap.EccUncorrectedTotal = 2
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEccErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "corrected")); got != 10 {
		t.Errorf("first poll corrected=%v want 10", got)
	}

	snap.EccCorrectedTotal = 17  // +7
	snap.EccUncorrectedTotal = 2 // unchanged
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEccErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "corrected")); got != 17 {
		t.Errorf("second poll corrected counter=%v want 17 (10 + 7 delta)", got)
	}
	if got := testutil.ToFloat64(deviceEccErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "uncorrected")); got != 2 {
		t.Errorf("uncorrected unchanged=%v want 2", got)
	}
}

func TestRecord_EccCounterResetTreatedAsCurrentValue(t *testing.T) {
	// 직전값 100 → 현재값 5 (드라이버 리셋 등) 인 경우 negative delta를 막고 current 자체를 적용한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.EccCorrectedTotal = 100
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEccErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "corrected")); got != 100 {
		t.Errorf("setup=%v want 100", got)
	}
	snap.EccCorrectedTotal = 5
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEccErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "corrected")); got != 105 {
		t.Errorf("post-reset=%v want 105 (100 + 5 fresh delta)", got)
	}
}

func TestRecord_UnsupportedFlagsSkipMetricEmission(t *testing.T) {
	// 모든 *Supported 플래그가 false면 base 5종(util / memory / temperature / power / mem-copy)만 발행되고
	// 신규 Phase 4 메트릭들은 series가 만들어지지 않아야 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.PcieSupported = false
	snap.ThrottleReasonsSupported = false
	snap.ClockSMSupported = false
	snap.ClockMemorySupported = false
	snap.ClockGraphicsSupported = false
	snap.EccSupported = false
	snap.EncoderSupported = false
	snap.DecoderSupported = false
	snap.PerformanceStateSupported = false
	Record("n", snap)

	mustZeroGauges := []*prometheus.GaugeVec{
		devicePcieRxBps, devicePcieTxBps,
		deviceThrottleActive, deviceClockMhz,
		deviceEncoderUtilization, deviceDecoderUtilization,
		devicePerformanceState,
	}
	for _, g := range mustZeroGauges {
		if got := testutil.CollectAndCount(g); got != 0 {
			t.Errorf("expected 0 series for unsupported metric; got %d", got)
		}
	}
	if got := testutil.CollectAndCount(deviceEccErrors); got != 0 {
		t.Errorf("ECC counter must remain empty when unsupported; got %d series", got)
	}
}
