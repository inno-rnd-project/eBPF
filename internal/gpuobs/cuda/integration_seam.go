//go:build integration

package cuda

import (
	"time"

	"netobs/internal/gpuobs/nvml"
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
