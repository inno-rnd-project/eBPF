package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// resetPodMetricsState는 패키지 레벨 podMemoryUsed gauge와 podMetricsEnabled 토글을
// 테스트마다 초기화해 case 간 라벨 누수가 일어나지 않도록 한다.
func resetPodMetricsState(t *testing.T) {
	t.Helper()
	podMemoryUsed.Reset()
	podMetricsEnabled = true
}

func TestRecordPod_WritesAllLabelsAndValue(t *testing.T) {
	resetPodMetricsState(t)

	dev := types.GPUDevice{Index: 1, UUID: "GPU-uuid-1", Model: "A100"}
	id := kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     "ml",
		PodName:       "trainer-0",
		PodUID:        "uid-xyz",
	}

	RecordPod("node-a", dev, id, 1234)

	got := testutil.ToFloat64(podMemoryUsed.WithLabelValues("node-a", "ml", "trainer-0", "uid-xyz", "GPU-uuid-1", "1"))
	if got != 1234 {
		t.Fatalf("podMemoryUsed=%v want 1234", got)
	}
}

func TestRecordPod_DisabledIsNoop(t *testing.T) {
	resetPodMetricsState(t)
	SetPodMetricsEnabled(false)
	t.Cleanup(func() { SetPodMetricsEnabled(true) })

	dev := types.GPUDevice{Index: 0, UUID: "GPU-uuid-1"}
	id := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ml", PodName: "p", PodUID: "u"}
	RecordPod("n", dev, id, 999)

	if got := testutil.CollectAndCount(podMemoryUsed); got != 0 {
		t.Fatalf("disabled toggle should skip recording; series count=%d", got)
	}
}

func TestRecordPod_NonPodIdentityIsNoop(t *testing.T) {
	resetPodMetricsState(t)

	dev := types.GPUDevice{Index: 0, UUID: "GPU-uuid-1"}
	cases := []kube.PodIdentity{
		{IdentityClass: kube.IdentityClassUnresolved},
		{IdentityClass: kube.IdentityClassNode, NodeName: "n1"},
		{IdentityClass: kube.IdentityClassExternal},
		{IdentityClass: kube.IdentityClassService},
		{}, // zero (=== unresolved per IsUnresolved)
	}
	for _, id := range cases {
		RecordPod("n", dev, id, 1)
	}

	if got := testutil.CollectAndCount(podMemoryUsed); got != 0 {
		t.Fatalf("non-pod identities should skip recording; series count=%d", got)
	}
}

func TestRecordPod_MissingPodNameAndUIDFallback(t *testing.T) {
	// Pod으로 분류되었지만 PodName/PodUID가 비어 있는 비정상 입력에서도 빈 라벨로 기록되지 않아야 한다.
	// "unknown" fallback이 적용되어 카디널리티 안전망 역할을 한다.
	resetPodMetricsState(t)

	dev := types.GPUDevice{Index: 0, UUID: "GPU-uuid-x"}
	id := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ml"}

	RecordPod("n", dev, id, 42)

	got := testutil.ToFloat64(podMemoryUsed.WithLabelValues("n", "ml", "unknown", "unknown", "GPU-uuid-x", "0"))
	if got != 42 {
		t.Fatalf("podMemoryUsed=%v want 42 (fallback labels)", got)
	}
}
