//go:build integration

package cuda

import (
	"time"

	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/kube"
)

// 본 파일은 통합 테스트 (internal/gpuobs/integration) 가 cuda 패키지의 사이클 단위 정합성을
// BPF kernel 호출 없이 검증할 수 있도록 노출하는 export-only 진입점을 모은다. //go:build integration
// 빌드 태그로 보호되어 production 바이너리 / 일반 단위 테스트 빌드에는 포함되지 않는다.

// NewIntegrationReader 는 통합 테스트가 외부에서 cuda.Reader 를 만들 때 사용하는 alias 다.
// 본 패키지의 New 가 libcudaPath / libcudartPath 를 요구하지만 통합 테스트는 attach 단계를
// 호출하지 않으므로 빈 문자열을 받는다.
func NewIntegrationReader(nodeName string, nv nvml.NVML, resolver PodResolver, refreshEvery time.Duration) *Reader {
	return New("", "", nodeName, nv, resolver, refreshEvery)
}

// RefreshOnceForTest 는 r.refreshOnce 의 통합 테스트용 export 다. 사이클 단위 정합성 (devicemap /
// podMap / visDev / RetainCudaSeries cleanup / dropped baseline) 을 한 호출로 진행한다.
func (r *Reader) RefreshOnceForTest(devSet *nvml.DeviceSet, devmap *DeviceMapForTest, dropped DroppedSourceForTest, baseline *DroppedBaseline) {
	r.refreshOnce(devSet, devmap.inner, droppedSourceShim{src: dropped}, &baseline.inner)
}

// DeviceMapForTest 는 internal deviceMap 의 export 래퍼다. 통합 테스트가 cycle 사이의 devmap
// 상태를 검증할 수 있게 한다.
type DeviceMapForTest struct {
	inner *deviceMap
}

// NewDeviceMapForTest 는 빈 DeviceMapForTest 를 반환한다.
func NewDeviceMapForTest() *DeviceMapForTest {
	return &DeviceMapForTest{inner: newDeviceMap()}
}

// LookupForTest 는 inner deviceMap 의 lookup 결과를 그대로 반환한다.
func (d *DeviceMapForTest) LookupForTest(pid uint32) string {
	return d.inner.lookup(pid)
}

// DroppedBaseline 은 internal droppedBaseline 의 export 래퍼다. 통합 테스트가 baseline 의
// 첫 사이클 확립과 reset 케이스를 검증할 수 있게 한다.
type DroppedBaseline struct {
	inner droppedBaseline
}

// Initialized 는 첫 사이클 통과 여부를 반환한다.
func (b *DroppedBaseline) Initialized() bool { return b.inner.initialized }

// DroppedSourceForTest 는 droppedSource 와 동일한 인터페이스다. cuda 패키지의 internal type 을
// 직접 노출하지 않고 통합 테스트가 자기 fake 를 정의해 주입할 수 있게 한다.
type DroppedSourceForTest interface {
	Total() uint64
}

// droppedSourceShim 은 DroppedSourceForTest 를 internal droppedSource 로 brigde 한다.
type droppedSourceShim struct {
	src DroppedSourceForTest
}

func (s droppedSourceShim) Total() uint64 { return s.src.Total() }

// RawEventForTest 는 통합 테스트가 BPF wire layout 을 직접 다루지 않고 dispatch 입력을 만들 수
// 있게 노출한 alias 다. CudaDeviceOrdUnknown 도 외부에서 사용 가능하게 같은 파일에 노출한다.
type RawEventForTest = rawEvent

// DispatchForTest 는 dispatch 의 export 다. 통합 테스트가 BPF ringbuf 를 mock 하지 않고
// dispatch 입력을 직접 만들어 hot path 의 캐시 hit / miss / visDev / devmap fallback 4 분기를
// 검증할 수 있게 한다.
func (r *Reader) DispatchForTest(raw RawEventForTest, devmap *DeviceMapForTest) {
	r.dispatch(raw, devmap.inner)
}

// ReplaceDevmapForTest 는 통합 테스트가 fake PID → UUID 매핑을 devmap 에 직접 적재할 때 사용한다.
// dispatch 의 폴백 경로 (devmap.lookup) 를 검증하기 위함.
func (d *DeviceMapForTest) ReplaceForTest(fresh map[uint32]string) {
	d.inner.replace(fresh)
}

// PreloadPodMapForTest 는 통합 테스트가 podMap 캐시를 미리 적재해 dispatch 가 ResolvePID 호출 없이
// 캐시 hit 경로로 진입하는 시나리오를 만들 때 사용한다.
func (r *Reader) PreloadPodMapForTest(fresh map[uint32]kube.PodIdentity) {
	r.pods.replace(fresh)
}

// PreloadVisDevForTest 는 visDev 캐시를 미리 적재해 dispatch 의 ordinal-to-UUID 변환 경로를
// 검증할 때 사용한다.
func (r *Reader) PreloadVisDevForTest(fresh map[uint32][]string) {
	r.visDev.replace(fresh)
}

// CudaEventSampleForTest 는 dispatch 가 recordEvent 로 발행하는 sample 을 통합 테스트가 직접
// capture 할 수 있도록 노출하는 alias 다.
type CudaEventSampleForTest struct {
	Node    string
	GPUUUID string
	Kind    uint8
	Bytes   uint64
	IsPod   bool
	PodName string
	PodUID  string
}

// CaptureRecordEventForTest 는 dispatch 가 호출하는 recordEvent 를 spy 로 교체해 capture 한
// sample 을 호출자 슬라이스에 기록한다. 통합 테스트가 dispatch hot path 의 분기 결과를 metric
// 쪽 변환 없이 직접 검증할 수 있게 한다.
func (r *Reader) CaptureRecordEventForTest(sink *[]CudaEventSampleForTest) {
	r.recordEvent = func(node string, sample metrics.CudaEventSample) {
		*sink = append(*sink, CudaEventSampleForTest{
			Node:    node,
			GPUUUID: sample.GPUUUID,
			Kind:    uint8(sample.Kind),
			Bytes:   sample.Bytes,
			IsPod:   sample.ID.IsPod(),
			PodName: sample.ID.PodName,
			PodUID:  sample.ID.PodUID,
		})
	}
}
