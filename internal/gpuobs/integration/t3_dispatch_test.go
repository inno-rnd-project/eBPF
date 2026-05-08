//go:build integration

package integration

import (
	"sync/atomic"
	"testing"
	"time"

	"netobs/internal/gpuobs/cuda"
	"netobs/internal/kube"
)

// TestT3_DispatchHotPathBranches 는 dispatch 의 4 가지 GPU UUID 결정 분기를 한 Reader 인스턴스
// 안에서 통과시켜 각 분기가 정확한 라벨을 산출하는지 검증한다.
//
// 분기:
//   1. cache hit: podMap 에 이미 적재된 PID → ResolvePID 호출 없이 visDev hit
//   2. cache miss: podMap 미적재 PID → ResolvePID 호출 후 visDev hit
//   3. visDev hit: BPF device_ord != UNKNOWN + visDev 매핑 있음 → ordinal-to-UUID 변환
//   4. devmap fallback: device_ord == UNKNOWN 또는 visDev miss → devmap.lookup 으로 폴백
func TestT3_DispatchHotPathBranches(t *testing.T) {
	resetCudaMetrics(t)

	resolver := &countingResolver{
		table: map[uint32]kube.PodIdentity{
			100: samplePod("ml", "p100", "u100"), // cache miss 시나리오
			200: samplePod("ml", "p200", "u200"), // cache hit 시나리오
		},
	}
	r := cuda.NewIntegrationReader("node-A", nil, resolver, time.Second)

	devmap := cuda.NewDeviceMapForTest()
	devmap.ReplaceForTest(map[uint32]string{
		100: "GPU-FB",
		200: "GPU-FB",
	})

	// 200 만 podMap 사전 적재 → 200 은 hit, 100 은 miss.
	r.PreloadPodMapForTest(map[uint32]kube.PodIdentity{
		200: samplePod("ml", "p200", "u200"),
	})

	// 200 의 visDev 매핑만 사전 적재 → ordinal 1 일 때 GPU-B 를 분리해서 발행해야 한다.
	r.PreloadVisDevForTest(map[uint32][]string{
		200: {"GPU-A", "GPU-B"},
	})

	var captured []cuda.CudaEventSampleForTest
	r.CaptureRecordEventForTest(&captured)

	// 분기 1 + 3: cache hit + visDev hit (PID 200, ordinal 1)
	r.DispatchForTest(cuda.RawEventForTest{PID: 200, Bytes: 4096, Kind: 2, DeviceOrd: 1}, devmap)

	// 분기 2: cache miss → ResolvePID 호출 + 적재. visDev 미적재라 devmap 폴백 (PID 100).
	r.DispatchForTest(cuda.RawEventForTest{PID: 100, Bytes: 1024, Kind: 1, DeviceOrd: 0}, devmap)

	// 분기 4: device_ord = UNKNOWN sentinel → 즉시 devmap fallback
	r.DispatchForTest(cuda.RawEventForTest{PID: 200, Bytes: 2048, Kind: 1, DeviceOrd: cuda.CudaDeviceOrdUnknown}, devmap)

	if len(captured) != 3 {
		t.Fatalf("captured=%d want 3", len(captured))
	}

	// 분기 1+3 검증: PID 200 첫 이벤트는 visDev ordinal 1 → GPU-B
	if got := captured[0]; got.GPUUUID != "GPU-B" || !got.IsPod || got.PodName != "p200" {
		t.Errorf("event 0 = %+v want GPU-B / p200 (cache hit + visDev hit)", got)
	}

	// 분기 2 검증: PID 100 은 visDev 미적재라 devmap fallback (GPU-FB)
	if got := captured[1]; got.GPUUUID != "GPU-FB" || !got.IsPod || got.PodName != "p100" {
		t.Errorf("event 1 = %+v want GPU-FB / p100 (cache miss + devmap fallback)", got)
	}

	// 분기 4 검증: UNKNOWN sentinel → devmap.lookup(200) = GPU-FB
	if got := captured[2]; got.GPUUUID != "GPU-FB" {
		t.Errorf("event 2 = %+v want GPU-FB (UNKNOWN sentinel)", got)
	}

	// ResolvePID 는 PID 100 만 호출되어야 한다 (PID 200 은 cache hit, UNKNOWN sentinel 도 podMap 캐시 사용).
	if got := resolver.count.Load(); got != 1 {
		t.Errorf("ResolvePID call count = %d want 1 (only PID 100 cache miss)", got)
	}
}

// countingResolver 는 ResolvePID 호출 횟수를 atomic 카운터로 추적한다.
type countingResolver struct {
	table map[uint32]kube.PodIdentity
	count atomic.Int64
}

func (c *countingResolver) ResolvePID(pid uint32) kube.PodIdentity {
	c.count.Add(1)
	return c.table[pid]
}
