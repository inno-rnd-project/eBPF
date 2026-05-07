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

// resetDeviceMetricsState는 패키지 레벨 device gauge/counter와 모든 delta 추적기를 초기화한다.
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
	deviceFanSpeed.Reset()
	deviceBAR1MemoryUsed.Reset()
	deviceBAR1MemoryTotal.Reset()
	devicePowerLimit.Reset()
	deviceThrottleViolationSeconds.Reset()
	deviceGpmUtilization.Reset()
	deviceEnergyConsumption.Reset()
	devicePcieLinkGenerationCurrent.Reset()
	devicePcieLinkWidthCurrent.Reset()
	devicePcieReplayErrors.Reset()
	deviceTemperatureThreshold.Reset()
	devicePowerLimitEnforced.Reset()
	devicePersistenceMode.Reset()
	deviceComputeMode.Reset()
	deviceInfo.Reset()
	deviceFirmwareInfo.Reset()
	lastEccAbsolute = make(map[string]uint64)
	lastViolationAbsolute = make(map[string]uint64)
	lastEnergyAbsolute = make(map[string]uint64)
	lastPcieReplayAbsolute = make(map[string]uint32)
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
		FanSpeedPct:               55,
		FanSpeedSupported:         true,
		BAR1MemoryUsedBytes:       64 * 1024 * 1024,
		BAR1MemoryTotalBytes:      256 * 1024 * 1024,
		BAR1Supported:             true,
		PowerLimitWatts:           350.0,
		PowerLimitSupported:       true,
		ViolationTimesNs: map[string]uint64{
			"power":   1_000_000_000, // 1s
			"thermal": 500_000_000,   // 0.5s
		},
		ViolationSupported:           true,
		GpmGraphicsUtilPct:           42.5,
		GpmSMOccupancyPct:            31.0,
		GpmTensorActivePct:           10.0,
		GpmDramBandwidthPct:          22.0,
		GpmSupported:                 true,
		GpmFirstSampleReady:          true,
		EnergyConsumptionMilliJoules: 0,
		EnergySupported:              true,
		PcieLinkGenerationCurrent:    4,
		PcieLinkWidthCurrent:         16,
		PcieLinkSupported:            true,
		PcieReplayErrors:             0,
		PcieReplaySupported:          true,
		TemperatureThresholdsCelsius: map[string]uint32{
			"slowdown": 90,
			"shutdown": 100,
			"mem_max":  95,
			"gpu_max":  93,
		},
		TemperatureThresholdSupported: true,
		PowerLimitEnforcedWatts:       350.0,
		PowerLimitEnforcedSupported:   true,
		PersistenceModeEnabled:        1,
		PersistenceModeSupported:      true,
		ComputeMode:                   0,
		ComputeModeSupported:          true,
	}
}

// fullySupportedSnapWithStaticInfo는 정적 정보까지 채워 deviceInfo / deviceFirmwareInfo 발행을 검증할 때 사용한다.
func fullySupportedSnapWithStaticInfo() types.GPUSnapshot {
	snap := fullySupportedSnap()
	snap.Device.CudaComputeMajor = 8
	snap.Device.CudaComputeMinor = 6
	snap.Device.Architecture = "ampere"
	snap.Device.MaxPcieLinkGeneration = 4
	snap.Device.MaxPcieLinkWidth = 16
	snap.Device.NumGpuCores = 10496
	snap.Device.MemoryBusWidthBits = 384
	snap.Device.VbiosVersion = "94.02.71.40.6e"
	snap.Device.GspFirmwareVersion = "550.54.15"
	return snap
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

func TestRecord_EccFirstPollIsBaselineOnly(t *testing.T) {
	// 첫 poll에 들어온 NVML VOLATILE 절대값(에이전트 기동 이전부터 노드 부팅 후 누적된 값)은
	// counter에 더해지지 않고 baseline으로만 저장된다. 이로써 _total counter가
	// "에이전트 기동 이후 신규 ECC 에러"의 정확한 의미를 가진다 (README 명시 의도와 일치).
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.EccCorrectedTotal = 1234
	snap.EccUncorrectedTotal = 567
	Record("n", snap)

	if got := testutil.CollectAndCount(deviceEccErrors); got != 0 {
		t.Errorf("first poll must not create any series; got %d", got)
	}
}

func TestRecord_EccDeltaTracking(t *testing.T) {
	// 첫 poll: baseline만 저장(counter 0). 두 번째 poll: 증가분만 적용.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.EccCorrectedTotal = 10
	snap.EccUncorrectedTotal = 2
	Record("n", snap)
	if got := testutil.CollectAndCount(deviceEccErrors); got != 0 {
		t.Errorf("first poll baseline-only; series=%d want 0", got)
	}

	snap.EccCorrectedTotal = 17  // +7 vs baseline 10
	snap.EccUncorrectedTotal = 2 // unchanged → delta 0, no Add
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEccErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "corrected")); got != 7 {
		t.Errorf("second poll corrected counter=%v want 7 (delta from baseline 10)", got)
	}
	// uncorrected는 delta 0이라 series 자체가 만들어지지 않아야 한다.
	if got := testutil.CollectAndCount(deviceEccErrors); got != 1 {
		t.Errorf("only corrected should have a series; got %d", got)
	}
}

func TestRecord_EccCounterResetSkipsAndRebaselines(t *testing.T) {
	// 첫 poll baseline=100, 두 번째 poll current=5 (드라이버 리셋 등) 인 경우 데이터 연속성 단절로 간주해
	// 해당 poll의 가산은 건너뛰고 5를 새 baseline 으로 둔다. 이후 정상 delta poll(8) 부터 가산이 재개된다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.EccCorrectedTotal = 100
	Record("n", snap)
	if got := testutil.CollectAndCount(deviceEccErrors); got != 0 {
		t.Errorf("first poll baseline-only; series=%d want 0", got)
	}

	snap.EccCorrectedTotal = 5 // reset (current < prev)
	Record("n", snap)
	if got := testutil.CollectAndCount(deviceEccErrors); got != 0 {
		t.Errorf("post-reset poll must skip Add and rebaseline; series=%d want 0", got)
	}

	snap.EccCorrectedTotal = 8 // +3 from new baseline 5
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEccErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "corrected")); got != 3 {
		t.Errorf("post-rebaseline counter=%v want 3 (delta from new baseline 5)", got)
	}
}

func TestRecord_UnsupportedFlagsSkipMetricEmission(t *testing.T) {
	// 모든 *Supported 플래그가 false여도 항상 발행되는 base 6 metric
	// (util / memory used / memory total / temperature / power / mem-copy) 은 그대로 유지되고,
	// *Supported 게이트에 묶인 신규 Phase 4 메트릭들은 series가 만들어지지 않아야 한다.
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
	snap.FanSpeedSupported = false
	snap.BAR1Supported = false
	snap.PowerLimitSupported = false
	snap.ViolationSupported = false
	snap.GpmSupported = false
	snap.EnergySupported = false
	snap.PcieLinkSupported = false
	snap.PcieReplaySupported = false
	snap.TemperatureThresholdSupported = false
	snap.PowerLimitEnforcedSupported = false
	snap.PersistenceModeSupported = false
	snap.ComputeModeSupported = false
	Record("n", snap)

	mustZeroGauges := []*prometheus.GaugeVec{
		devicePcieRxBps, devicePcieTxBps,
		deviceThrottleActive, deviceClockMhz,
		deviceEncoderUtilization, deviceDecoderUtilization,
		devicePerformanceState,
		deviceFanSpeed, deviceBAR1MemoryUsed, deviceBAR1MemoryTotal,
		devicePowerLimit, deviceGpmUtilization,
		devicePcieLinkGenerationCurrent, devicePcieLinkWidthCurrent,
		deviceTemperatureThreshold, devicePowerLimitEnforced,
		devicePersistenceMode, deviceComputeMode,
	}
	for _, g := range mustZeroGauges {
		if got := testutil.CollectAndCount(g); got != 0 {
			t.Errorf("expected 0 series for unsupported metric; got %d", got)
		}
	}
	if got := testutil.CollectAndCount(deviceEccErrors); got != 0 {
		t.Errorf("ECC counter must remain empty when unsupported; got %d series", got)
	}
	if got := testutil.CollectAndCount(deviceThrottleViolationSeconds); got != 0 {
		t.Errorf("Violation counter must remain empty when unsupported; got %d series", got)
	}
	if got := testutil.CollectAndCount(deviceEnergyConsumption); got != 0 {
		t.Errorf("Energy counter must remain empty when unsupported; got %d series", got)
	}
	if got := testutil.CollectAndCount(devicePcieReplayErrors); got != 0 {
		t.Errorf("PCIe replay counter must remain empty when unsupported; got %d series", got)
	}
}

func TestRecord_FanSpeedAndBAR1AndPowerLimit(t *testing.T) {
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	Record("n", snap)

	if got := testutil.ToFloat64(deviceFanSpeed.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 55 {
		t.Errorf("fan speed=%v want 55", got)
	}
	if got := testutil.ToFloat64(deviceBAR1MemoryUsed.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != float64(64*1024*1024) {
		t.Errorf("bar1 used=%v", got)
	}
	if got := testutil.ToFloat64(deviceBAR1MemoryTotal.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != float64(256*1024*1024) {
		t.Errorf("bar1 total=%v", got)
	}
	if got := testutil.ToFloat64(devicePowerLimit.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 350.0 {
		t.Errorf("power limit=%v want 350", got)
	}
}

func TestRecord_GpmFirstSampleSkipped(t *testing.T) {
	// 첫 sample만 받은 직후 (FirstSampleReady=false) GPM 4종 series가 만들어지지 않아야 한다.
	// nvml 계층의 GPM ping-pong 패턴이 metrics 발행 단계와 정확히 맞물려야 함을 고정한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.GpmFirstSampleReady = false
	Record("n", snap)

	if got := testutil.CollectAndCount(deviceGpmUtilization); got != 0 {
		t.Errorf("first GPM sample must not emit metrics; series=%d", got)
	}
}

func TestRecord_GpmAllFourMetricsLabeled(t *testing.T) {
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	Record("n", snap)

	if got := testutil.ToFloat64(deviceGpmUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "graphics_util")); got != 42.5 {
		t.Errorf("gpm graphics_util=%v want 42.5", got)
	}
	if got := testutil.ToFloat64(deviceGpmUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "sm_occupancy")); got != 31.0 {
		t.Errorf("gpm sm_occupancy=%v want 31", got)
	}
	if got := testutil.ToFloat64(deviceGpmUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "tensor_active")); got != 10.0 {
		t.Errorf("gpm tensor_active=%v want 10", got)
	}
	if got := testutil.ToFloat64(deviceGpmUtilization.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "dram_bandwidth")); got != 22.0 {
		t.Errorf("gpm dram_bandwidth=%v want 22", got)
	}
	if got := testutil.CollectAndCount(deviceGpmUtilization); got != 4 {
		t.Errorf("expected 4 GPM series (one per metric label); got %d", got)
	}
}

func TestRecord_ViolationFirstPollIsBaselineOnly(t *testing.T) {
	// ECC와 동일하게 첫 poll의 NVML 누적값(부팅 후 누적)은 counter에 더해지지 않고 baseline으로만 저장된다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.ViolationTimesNs = map[string]uint64{"power": 5_000_000_000}
	Record("n", snap)

	if got := testutil.CollectAndCount(deviceThrottleViolationSeconds); got != 0 {
		t.Errorf("first poll must not create any violation series; got %d", got)
	}
}

func TestRecord_ViolationDeltaConvertedToSeconds(t *testing.T) {
	// nanoseconds 단위 NVML 누적값에서 직전 poll 값을 빼 양수 delta만 seconds로 환산해 Counter.Add 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.ViolationTimesNs = map[string]uint64{"power": 2_000_000_000} // 2s baseline
	Record("n", snap)

	snap.ViolationTimesNs = map[string]uint64{"power": 5_000_000_000} // +3s
	Record("n", snap)

	if got := testutil.ToFloat64(deviceThrottleViolationSeconds.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "power")); got != 3 {
		t.Errorf("violation power counter=%v want 3 seconds (delta 3e9 ns)", got)
	}
}

func TestRecord_ViolationCounterResetSkipsAndRebaselines(t *testing.T) {
	// baseline 100ms, 두 번째 poll 50ms 인 경우 reset으로 간주해 가산을 건너뛰고 50ms를 새 baseline 으로 둔다.
	// 이후 정상 delta poll(70ms) 에서 +20ms = 0.02s 가 가산되어야 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.ViolationTimesNs = map[string]uint64{"thermal": 100_000_000} // 100ms baseline
	Record("n", snap)

	snap.ViolationTimesNs = map[string]uint64{"thermal": 50_000_000} // 50ms (reset)
	Record("n", snap)
	if got := testutil.CollectAndCount(deviceThrottleViolationSeconds); got != 0 {
		t.Errorf("post-reset poll must skip and rebaseline; series=%d want 0", got)
	}

	snap.ViolationTimesNs = map[string]uint64{"thermal": 70_000_000} // +20ms from new baseline 50ms
	Record("n", snap)
	if got := testutil.ToFloat64(deviceThrottleViolationSeconds.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "thermal")); got != 0.02 {
		t.Errorf("post-rebaseline violation counter=%v want 0.02 (delta 20ms from new baseline)", got)
	}
}

func TestRecord_EnergyCounterResetSkipsAndRebaselines(t *testing.T) {
	// baseline 1_000_000_000 mJ, 두 번째 poll 500 mJ (driver reload) — 가산 skip + rebaseline.
	// 이후 1_500 mJ poll에서 +1000 mJ = 1 J 가 가산되어야 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.EnergyConsumptionMilliJoules = 1_000_000_000
	Record("n", snap)

	snap.EnergyConsumptionMilliJoules = 500 // reset
	Record("n", snap)
	if got := testutil.CollectAndCount(deviceEnergyConsumption); got != 0 {
		t.Errorf("post-reset poll must skip and rebaseline; series=%d want 0", got)
	}

	snap.EnergyConsumptionMilliJoules = 1_500 // +1000 mJ from new baseline 500
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEnergyConsumption.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 1 {
		t.Errorf("post-rebaseline energy counter=%v want 1 J", got)
	}
}

func TestRecord_PcieReplayCounterResetSkipsAndRebaselines(t *testing.T) {
	// baseline 100, 두 번째 poll 5 (wrap 또는 reset) — 가산 skip + rebaseline.
	// 이후 8 poll에서 +3 가 가산되어야 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.PcieReplayErrors = 100
	Record("n", snap)

	snap.PcieReplayErrors = 5 // reset
	Record("n", snap)
	if got := testutil.CollectAndCount(devicePcieReplayErrors); got != 0 {
		t.Errorf("post-reset poll must skip and rebaseline; series=%d want 0", got)
	}

	snap.PcieReplayErrors = 8 // +3 from new baseline 5
	Record("n", snap)
	if got := testutil.ToFloat64(devicePcieReplayErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 3 {
		t.Errorf("post-rebaseline pcie replay counter=%v want 3", got)
	}
}

func TestRecord_EnergyDeltaConvertedToJoules(t *testing.T) {
	// NVML 누적 mJ 절대값에서 직전 poll 값을 빼 양수 delta만 J로 환산해 Counter.Add 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.EnergyConsumptionMilliJoules = 1_000_000 // 1000J baseline (1 megajoule worth of mJ)
	Record("n", snap)
	if got := testutil.CollectAndCount(deviceEnergyConsumption); got != 0 {
		t.Errorf("first poll baseline-only; series=%d want 0", got)
	}

	snap.EnergyConsumptionMilliJoules = 1_500_000 // +500_000 mJ = +500 J
	Record("n", snap)
	if got := testutil.ToFloat64(deviceEnergyConsumption.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 500 {
		t.Errorf("energy counter=%v want 500 J (delta 500000mJ)", got)
	}
}

func TestRecord_PcieReplayDelta(t *testing.T) {
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	snap.PcieReplayErrors = 100 // baseline
	Record("n", snap)
	if got := testutil.CollectAndCount(devicePcieReplayErrors); got != 0 {
		t.Errorf("first poll baseline-only; series=%d want 0", got)
	}

	snap.PcieReplayErrors = 105 // +5
	Record("n", snap)
	if got := testutil.ToFloat64(devicePcieReplayErrors.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 5 {
		t.Errorf("pcie replay counter=%v want 5", got)
	}
}

func TestRecord_PcieLinkAndPowerLimitEnforcedAndModes(t *testing.T) {
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	Record("n", snap)

	if got := testutil.ToFloat64(devicePcieLinkGenerationCurrent.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 4 {
		t.Errorf("pcie link gen=%v want 4", got)
	}
	if got := testutil.ToFloat64(devicePcieLinkWidthCurrent.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 16 {
		t.Errorf("pcie link width=%v want 16", got)
	}
	if got := testutil.ToFloat64(devicePowerLimitEnforced.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 350 {
		t.Errorf("enforced power limit=%v want 350", got)
	}
	if got := testutil.ToFloat64(devicePersistenceMode.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 1 {
		t.Errorf("persistence mode=%v want 1", got)
	}
	if got := testutil.ToFloat64(deviceComputeMode.WithLabelValues("n", "GPU-A", "0", "RTX-3090")); got != 0 {
		t.Errorf("compute mode=%v want 0 (Default)", got)
	}
}

func TestRecord_TemperatureThresholdsLabeled(t *testing.T) {
	// 4종 threshold 모두 별도 시리즈로 발행되어야 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap()
	Record("n", snap)

	if got := testutil.CollectAndCount(deviceTemperatureThreshold); got != 4 {
		t.Errorf("expected 4 threshold series; got %d", got)
	}
	if got := testutil.ToFloat64(deviceTemperatureThreshold.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "slowdown")); got != 90 {
		t.Errorf("slowdown threshold=%v want 90", got)
	}
	if got := testutil.ToFloat64(deviceTemperatureThreshold.WithLabelValues("n", "GPU-A", "0", "RTX-3090", "shutdown")); got != 100 {
		t.Errorf("shutdown threshold=%v want 100", got)
	}
}

func TestRecord_DeviceInfoAndFirmwareInfo(t *testing.T) {
	// 정적 라벨이 채워진 snapshot에서 deviceInfo / deviceFirmwareInfo 가 정확한 라벨 값으로 1을 발행한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnapWithStaticInfo()
	Record("n", snap)

	if got := testutil.ToFloat64(deviceInfo.WithLabelValues(
		"n", "GPU-A", "0", "RTX-3090",
		"8.6", "ampere", "4", "16", "10496", "384",
	)); got != 1 {
		t.Errorf("device_info=%v want 1 with full static labels", got)
	}
	if got := testutil.ToFloat64(deviceFirmwareInfo.WithLabelValues(
		"n", "GPU-A", "0", "RTX-3090",
		"94.02.71.40.6e", "550.54.15",
	)); got != 1 {
		t.Errorf("device_firmware_info=%v want 1 with vbios/gsp", got)
	}
}

func TestRecord_DeviceInfoFallbackForMissingFields(t *testing.T) {
	// 정적 정보 수집이 실패해 zero value인 필드들은 "unknown"/0으로 라벨에 들어가야 한다.
	resetDeviceMetricsState(t)

	snap := fullySupportedSnap() // static fields 모두 zero value
	Record("n", snap)

	// compute_capability="unknown", architecture="unknown", 정수 필드들은 "0".
	if got := testutil.ToFloat64(deviceInfo.WithLabelValues(
		"n", "GPU-A", "0", "RTX-3090",
		"unknown", "unknown", "0", "0", "0", "0",
	)); got != 1 {
		t.Errorf("device_info fallback labels=%v want 1", got)
	}
}

// --------------------- cuda uprobe 메트릭 ---------------------

// resetCudaMetricsState 는 cuda 카운터 / 심볼 가용성 gauge / lost counter / seenCudaKeys 추적기를 초기화한다.
func resetCudaMetricsState(t *testing.T) {
	t.Helper()
	cudaKernelLaunchesTotal.Reset()
	cudaH2DBytesTotal.Reset()
	cudaD2HBytesTotal.Reset()
	cudaDtoDBytesTotal.Reset()
	cudaUnknownDirBytesTotal.Reset()
	cudaSymbolAvailable.Reset()
	cudaEventsLostTotal.Reset()
	cudaPidMultiGPUCount.Reset()
	seenCudaKeys = make(map[CudaLabelKey]struct{})
}

func TestRecordCudaEvent_KernelLaunchIncrementsCounter(t *testing.T) {
	resetCudaMetricsState(t)

	id := samplePod("ml", "trainer-0", "uid-xyz")
	RecordCudaEvent("n", CudaEventSample{
		ID:      id,
		GPUUUID: "GPU-1",
		Kind:    types.CudaEventKernelLaunch,
	})
	RecordCudaEvent("n", CudaEventSample{
		ID:      id,
		GPUUUID: "GPU-1",
		Kind:    types.CudaEventKernelLaunch,
	})

	if got := testutil.ToFloat64(cudaKernelLaunchesTotal.WithLabelValues("n", "ml", "trainer-0", "uid-xyz", "GPU-1")); got != 2 {
		t.Fatalf("kernel launches=%v want 2", got)
	}
}

func TestRecordCudaEvent_H2DAndD2HAccumulateBytes(t *testing.T) {
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventH2D, Bytes: 1024})
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventH2D, Bytes: 2048})
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventD2H, Bytes: 512})

	if got := testutil.ToFloat64(cudaH2DBytesTotal.WithLabelValues("n", "ml", "p", "u", "G")); got != 3072 {
		t.Errorf("h2d bytes=%v want 3072", got)
	}
	if got := testutil.ToFloat64(cudaD2HBytesTotal.WithLabelValues("n", "ml", "p", "u", "G")); got != 512 {
		t.Errorf("d2h bytes=%v want 512", got)
	}
}

func TestRecordCudaEvent_UnknownDirAccumulatesBytes(t *testing.T) {
	// UVA cuMemcpy 와 2D/3D 의 ARRAY / UNIFIED / HOST→HOST 케이스가 합류하는 unknown_dir 카운터 검증.
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventUnknownDir, Bytes: 4096})
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventUnknownDir, Bytes: 8192})

	if got := testutil.ToFloat64(cudaUnknownDirBytesTotal.WithLabelValues("n", "ml", "p", "u", "G")); got != 12288 {
		t.Errorf("unknown_dir bytes=%v want 12288", got)
	}
	if got := testutil.CollectAndCount(cudaH2DBytesTotal); got != 0 {
		t.Errorf("unknown_dir must not leak into h2d; got %d series", got)
	}
}

func TestRetainCudaSeries_RemovesUnknownDirStaleSeries(t *testing.T) {
	resetCudaMetricsState(t)

	pod := samplePod("ml", "a", "uid-a")
	RecordCudaEvent("n", CudaEventSample{ID: pod, GPUUUID: "G", Kind: types.CudaEventUnknownDir, Bytes: 1000})
	if got := testutil.CollectAndCount(cudaUnknownDirBytesTotal); got != 1 {
		t.Fatalf("setup: unknown_dir series=%d want 1", got)
	}

	RetainCudaSeries(map[CudaLabelKey]struct{}{})
	if got := testutil.CollectAndCount(cudaUnknownDirBytesTotal); got != 0 {
		t.Errorf("after empty active set unknown_dir series=%d want 0", got)
	}
}

func TestRecordCudaEvent_DtoDAccumulatesBytes(t *testing.T) {
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventDtoD, Bytes: 8192})
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventDtoD, Bytes: 4096})

	if got := testutil.ToFloat64(cudaDtoDBytesTotal.WithLabelValues("n", "ml", "p", "u", "G")); got != 12288 {
		t.Errorf("dtod bytes=%v want 12288", got)
	}
	// 다른 kind 의 카운터에는 영향이 없어야 한다.
	if got := testutil.CollectAndCount(cudaH2DBytesTotal); got != 0 {
		t.Errorf("dtod must not leak into h2d; got %d series", got)
	}
	if got := testutil.CollectAndCount(cudaD2HBytesTotal); got != 0 {
		t.Errorf("dtod must not leak into d2h; got %d series", got)
	}
}

func TestRetainCudaSeries_RemovesDtoDStaleSeries(t *testing.T) {
	// dtod 시리즈도 RetainCudaSeries 에서 cleanup 되어야 한다 — 종료된 (Pod, GPU) 가 active 셋에 없으면
	// kernel / h2d / d2h 와 동일하게 surgical Delete 된다.
	resetCudaMetricsState(t)

	podA := samplePod("ml", "a", "uid-a")
	podB := samplePod("ml", "b", "uid-b")
	RecordCudaEvent("n", CudaEventSample{ID: podA, GPUUUID: "G", Kind: types.CudaEventDtoD, Bytes: 100})
	RecordCudaEvent("n", CudaEventSample{ID: podB, GPUUUID: "G", Kind: types.CudaEventDtoD, Bytes: 200})

	if got := testutil.CollectAndCount(cudaDtoDBytesTotal); got != 2 {
		t.Fatalf("setup: dtod series=%d want 2", got)
	}

	active := map[CudaLabelKey]struct{}{
		CudaActiveKey("n", "ml", "a", "uid-a", "G"): {},
	}
	RetainCudaSeries(active)

	if got := testutil.CollectAndCount(cudaDtoDBytesTotal); got != 1 {
		t.Errorf("after cleanup dtod series=%d want 1 (podA only)", got)
	}
	if got := testutil.ToFloat64(cudaDtoDBytesTotal.WithLabelValues("n", "ml", "a", "uid-a", "G")); got != 100 {
		t.Errorf("podA dtod counter=%v want 100 (preserved)", got)
	}
}

func TestRecordCudaEvent_NonPodIdentitySkipped(t *testing.T) {
	// Pod 으로 분류되지 않은 식별자(호스트 프로세스 / 미해상도 등) 는 발행을 건너뛰어야 한다.
	// RecordPodSnapshot 의 IsPod 게이트와 동일 정책.
	resetCudaMetricsState(t)

	cases := []kube.PodIdentity{
		{IdentityClass: kube.IdentityClassUnresolved},
		{IdentityClass: kube.IdentityClassNode, NodeName: "n1"},
		{IdentityClass: kube.IdentityClassExternal},
		{IdentityClass: kube.IdentityClassService},
		{},
	}
	for _, id := range cases {
		RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})
	}
	if got := testutil.CollectAndCount(cudaKernelLaunchesTotal); got != 0 {
		t.Fatalf("non-pod identities must not be recorded; series=%d", got)
	}
}

func TestRecordCudaEvent_MissingGPUUUIDFallsBackToUnknown(t *testing.T) {
	// PID→GPU 매핑 실패 시 GPUUUID 가 빈 문자열로 들어올 수 있고, 그대로 라벨로 노출되면
	// 카디널리티가 빈 값으로 늘어난다. "unknown" fallback 으로 격리한다.
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "", Kind: types.CudaEventKernelLaunch})

	if got := testutil.ToFloat64(cudaKernelLaunchesTotal.WithLabelValues("n", "ml", "p", "u", "unknown")); got != 1 {
		t.Errorf("missing gpu_uuid must fallback to 'unknown'; got %v want 1", got)
	}
}

func TestRecordCudaEvent_MissingPodNameAndUIDFallback(t *testing.T) {
	// Pod 이지만 PodName/PodUID 가 비어 있는 비정상 입력에서도 빈 라벨로 기록되지 않아야 한다.
	resetCudaMetricsState(t)

	id := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ml"}
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})

	if got := testutil.ToFloat64(cudaKernelLaunchesTotal.WithLabelValues("n", "ml", "unknown", "unknown", "G")); got != 1 {
		t.Errorf("fallback labels expected; got %v want 1", got)
	}
}

func TestRecordCudaEvent_UnknownKindSkipped(t *testing.T) {
	// BPF / userspace enum 이 어긋나 정의되지 않은 kind 가 들어오면 발행을 건너뛴다.
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventKind(99)})

	if got := testutil.CollectAndCount(cudaKernelLaunchesTotal); got != 0 {
		t.Errorf("unknown kind must not create any series; got %d", got)
	}
	if got := testutil.CollectAndCount(cudaH2DBytesTotal); got != 0 {
		t.Errorf("unknown kind must not create any series; got %d", got)
	}
	if got := testutil.CollectAndCount(cudaD2HBytesTotal); got != 0 {
		t.Errorf("unknown kind must not create any series; got %d", got)
	}
}

func TestRetainCudaSeries_RemovesStaleAndKeepsActive(t *testing.T) {
	// podA, podB 둘 다 이벤트가 들어온 뒤, 다음 cleanup 에서 podA 만 active 로 보고되면
	// podB 의 3 family 시리즈가 모두 제거되고 podA 시리즈는 유지되어야 한다.
	resetCudaMetricsState(t)

	podA := samplePod("ml", "a", "uid-a")
	podB := samplePod("ml", "b", "uid-b")
	RecordCudaEvent("n", CudaEventSample{ID: podA, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})
	RecordCudaEvent("n", CudaEventSample{ID: podA, GPUUUID: "G", Kind: types.CudaEventH2D, Bytes: 100})
	RecordCudaEvent("n", CudaEventSample{ID: podA, GPUUUID: "G", Kind: types.CudaEventD2H, Bytes: 50})
	RecordCudaEvent("n", CudaEventSample{ID: podB, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})
	RecordCudaEvent("n", CudaEventSample{ID: podB, GPUUUID: "G", Kind: types.CudaEventH2D, Bytes: 200})

	if got := testutil.CollectAndCount(cudaKernelLaunchesTotal); got != 2 {
		t.Fatalf("setup: kernel series=%d want 2 (podA+podB)", got)
	}

	active := map[CudaLabelKey]struct{}{
		CudaActiveKey("n", "ml", "a", "uid-a", "G"): {},
	}
	RetainCudaSeries(active)

	if got := testutil.CollectAndCount(cudaKernelLaunchesTotal); got != 1 {
		t.Errorf("kernel series after cleanup=%d want 1 (podA only)", got)
	}
	if got := testutil.CollectAndCount(cudaH2DBytesTotal); got != 1 {
		t.Errorf("h2d series after cleanup=%d want 1 (podA only)", got)
	}
	if got := testutil.CollectAndCount(cudaD2HBytesTotal); got != 1 {
		t.Errorf("d2h series after cleanup=%d want 1 (podA only, podB never had d2h)", got)
	}
	// podA 카운터 값은 유지되어야 한다 (Reset 이 아닌 surgical Delete).
	if got := testutil.ToFloat64(cudaKernelLaunchesTotal.WithLabelValues("n", "ml", "a", "uid-a", "G")); got != 1 {
		t.Errorf("podA kernel counter value=%v want 1 (preserved)", got)
	}
}

func TestRetainCudaSeries_EmptyActiveCleansAll(t *testing.T) {
	// 모든 GPU 워크로드가 종료되어 빈 active 가 들어오면 직전까지 기록된 모든 cuda 시리즈가 제거되어야 한다.
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventH2D, Bytes: 10})

	RetainCudaSeries(map[CudaLabelKey]struct{}{})

	if got := testutil.CollectAndCount(cudaKernelLaunchesTotal); got != 0 {
		t.Errorf("kernel series=%d want 0", got)
	}
	if got := testutil.CollectAndCount(cudaH2DBytesTotal); got != 0 {
		t.Errorf("h2d series=%d want 0", got)
	}
}

func TestRetainCudaSeries_RebuildsAfterCleanup(t *testing.T) {
	// cleanup 으로 시리즈 / seenCudaKeys 가 비워진 후 다시 같은 라벨로 이벤트가 들어오면
	// 정상적으로 신규 시리즈가 만들어져야 한다 (seenCudaKeys 가 영구히 stale 키를 남기지 않음).
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})
	RetainCudaSeries(map[CudaLabelKey]struct{}{})
	if got := testutil.CollectAndCount(cudaKernelLaunchesTotal); got != 0 {
		t.Fatalf("setup: cleanup expected; series=%d", got)
	}

	RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})
	if got := testutil.ToFloat64(cudaKernelLaunchesTotal.WithLabelValues("n", "ml", "p", "u", "G")); got != 1 {
		t.Errorf("re-record after cleanup counter=%v want 1 (counter restarts at 0 then +1)", got)
	}
}

func TestRecordCudaEvent_FastPathSkipsWriteLockOnRepeatedKey(t *testing.T) {
	// 같은 (Pod, GPU) 라벨 키로 두 번째 이후 호출은 fast path (RLock-only) 를 타야 한다.
	// 기능 정합성 측면에서: 두 번째 호출 후에도 seenCudaKeys 에 키가 정확히 1개 남고 카운터는 누적되어야 한다.
	resetCudaMetricsState(t)

	id := samplePod("ml", "p", "u")
	for i := 0; i < 5; i++ {
		RecordCudaEvent("n", CudaEventSample{ID: id, GPUUUID: "G", Kind: types.CudaEventKernelLaunch})
	}

	if got := testutil.ToFloat64(cudaKernelLaunchesTotal.WithLabelValues("n", "ml", "p", "u", "G")); got != 5 {
		t.Errorf("counter=%v want 5 (5 events on the same key)", got)
	}
	if got := len(seenCudaKeys); got != 1 {
		t.Errorf("seenCudaKeys size=%d want 1 (fast path must not duplicate)", got)
	}
}

func TestAddCudaEventsLost_AccumulatesByNode(t *testing.T) {
	resetCudaMetricsState(t)

	AddCudaEventsLost("n1", 3)
	AddCudaEventsLost("n1", 7)
	AddCudaEventsLost("n2", 2)

	if got := testutil.ToFloat64(cudaEventsLostTotal.WithLabelValues("n1")); got != 10 {
		t.Errorf("n1 lost=%v want 10", got)
	}
	if got := testutil.ToFloat64(cudaEventsLostTotal.WithLabelValues("n2")); got != 2 {
		t.Errorf("n2 lost=%v want 2", got)
	}
}

func TestSetCudaSymbolAvailability_BothStates(t *testing.T) {
	resetCudaMetricsState(t)

	SetCudaSymbolAvailability("n", "cuLaunchKernel", true)
	SetCudaSymbolAvailability("n", "cuMemcpyHtoD_v2", false)

	if got := testutil.ToFloat64(cudaSymbolAvailable.WithLabelValues("n", "cuLaunchKernel")); got != 1 {
		t.Errorf("cuLaunchKernel=%v want 1", got)
	}
	if got := testutil.ToFloat64(cudaSymbolAvailable.WithLabelValues("n", "cuMemcpyHtoD_v2")); got != 0 {
		t.Errorf("cuMemcpyHtoD_v2=%v want 0", got)
	}
	// 같은 (node, symbol) 로 다시 호출하면 idempotent Set 으로 시리즈가 늘어나지 않아야 한다.
	SetCudaSymbolAvailability("n", "cuLaunchKernel", false)
	if got := testutil.ToFloat64(cudaSymbolAvailable.WithLabelValues("n", "cuLaunchKernel")); got != 0 {
		t.Errorf("cuLaunchKernel after re-set=%v want 0", got)
	}
	if got := testutil.CollectAndCount(cudaSymbolAvailable); got != 2 {
		t.Errorf("series count=%d want 2", got)
	}
}

func TestSetCudaPidMultiGPUCount_OverwritesByNode(t *testing.T) {
	resetCudaMetricsState(t)

	SetCudaPidMultiGPUCount("n1", 0)
	if got := testutil.ToFloat64(cudaPidMultiGPUCount.WithLabelValues("n1")); got != 0 {
		t.Errorf("initial n1=%v want 0", got)
	}

	SetCudaPidMultiGPUCount("n1", 3)
	if got := testutil.ToFloat64(cudaPidMultiGPUCount.WithLabelValues("n1")); got != 3 {
		t.Errorf("n1=%v want 3", got)
	}

	// 새 사이클이 multi-GPU PID 수를 다시 0 으로 발행하면 idempotent Set 으로 같은 시리즈에 덮어써져야 한다.
	SetCudaPidMultiGPUCount("n1", 0)
	if got := testutil.ToFloat64(cudaPidMultiGPUCount.WithLabelValues("n1")); got != 0 {
		t.Errorf("n1 after reset=%v want 0", got)
	}
	if got := testutil.CollectAndCount(cudaPidMultiGPUCount); got != 1 {
		t.Errorf("series count=%d want 1 (single node)", got)
	}
}
