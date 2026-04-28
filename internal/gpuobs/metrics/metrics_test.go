package metrics

import (
	"testing"

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
