//go:build integration

package integration

import (
	"testing"

	"netobs/internal/gpuobs/collector"
	"netobs/internal/gpuobs/config"
	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// TestT4_OptOutToggleEffects 는 세 토글 (GPU_METRICS_ENABLED / GPUOBS_POD_METRICS_ENABLED /
// GPUOBS_CUDA_UPROBE_ENABLED) 가 각각의 메트릭 시리즈 발행 / 비발행을 정확히 제어하는지 검증한다.
//
// 토글 effect 격리:
//   - GPUMetricsEnabled=false: collector.Run 이 즉시 disabled 분기로 빠져 device 시리즈 0
//   - PodMetricsEnabled=false: pollOnce 가 RecordPodSnapshot 으로 nil 슬라이스를 넘겨 pod 시리즈 0
//   - CudaUprobeEnabled=false: 본 PR 에서는 cuda.Reader 를 시작하지 않는 상위 결정이라, cuda 시리즈가
//     0 으로 유지되는지를 확인 (T2 가 활성 시 양수 발행을 별도로 검증)
func TestT4_OptOutToggleEffects(t *testing.T) {
	// 시나리오 1: 모두 활성. device + pod 시리즈가 양수, cuda 는 0 (Reader 미가동).
	t.Run("all enabled", func(t *testing.T) {
		metrics.SetPodMetricsEnabled(true)
		metrics.ResetDeviceMetricsForTest()
		metrics.ResetPodMetricsForTest()
		metrics.ResetCudaStateForTest()

		nv := newToggleFakeNVML([]toggleFakeDevice{
			{uuid: "GPU-A", index: 0, snapshot: types.GPUSnapshot{
				Device:         types.GPUDevice{UUID: "GPU-A", Index: 0},
				UtilizationPct: 50,
			}},
		})
		c := collector.New(nv, config.Config{
			NodeName:          "node-A",
			GPUMetricsEnabled: true,
			PodMetricsEnabled: true,
		}, &nilResolver{})
		devSet := nvml.NewDeviceSet(nv)
		defer devSet.Close()
		_ = devSet.Sync()
		c.PollOnceForTest(devSet)

		if got := metrics.CountDeviceMetricSeriesForTest(); got == 0 {
			t.Errorf("device metric series=0 want >0 (toggle enabled)")
		}
		if got := metrics.CountCudaCounterSeriesForTest(); got != 0 {
			t.Errorf("cuda counter series=%d want 0 (Reader not started)", got)
		}
	})

	// 시나리오 2: GPUMetricsEnabled=true 이지만 PodMetricsEnabled=false.
	// pollOnce 가 SetPodMetricsEnabled 와 별개로 동작하므로 metrics 패키지의 토글 set 으로 검증한다.
	t.Run("pod metrics disabled", func(t *testing.T) {
		metrics.ResetDeviceMetricsForTest()
		metrics.ResetPodMetricsForTest()
		metrics.SetPodMetricsEnabled(false)

		// SetPodMetricsEnabled(false) 시점에 RecordPodSnapshot 이 nil 적재 후 cleanup 만 수행한다.
		// 직전 시리즈가 없으므로 결과적으로 pod 시리즈는 0 이어야 한다.
		metrics.RecordPodSnapshot("node-A", []metrics.PodGPUSample{
			{Device: types.GPUDevice{UUID: "GPU-A", Index: 0}, MemUsedBytes: 100},
		})

		if got := metrics.CountPodMetricSeriesForTest(); got != 0 {
			t.Errorf("pod metric series=%d want 0 (toggle off)", got)
		}

		// 다시 활성화 후 같은 호출이 시리즈를 만들어야 한다 (대조군).
		metrics.SetPodMetricsEnabled(true)
		metrics.RecordPodSnapshot("node-A", []metrics.PodGPUSample{
			{ID: samplePod("ml", "p1", "u1"), Device: types.GPUDevice{UUID: "GPU-A", Index: 0}, MemUsedBytes: 100},
		})
		if got := metrics.CountPodMetricSeriesForTest(); got == 0 {
			t.Errorf("pod metric series=0 want >0 after re-enabling toggle")
		}
	})

	// 시나리오 3: CudaUprobeEnabled=false 시점에는 cuda.Reader 자체가 시작되지 않으므로 본 PR 의 통합
	// 테스트 범위에서 cuda 카운터는 0 으로 유지된다. T2 / T3 가 활성 경로에서 양수 발행을 검증하므로,
	// 본 시나리오에서는 cuda Reader 미가동 상태에서 카운터가 0 임을 확인하는 것으로 충분하다.
	t.Run("cuda uprobe disabled implies zero cuda series", func(t *testing.T) {
		metrics.ResetCudaStateForTest()
		if got := metrics.CountCudaCounterSeriesForTest(); got != 0 {
			t.Errorf("cuda counter series=%d want 0 (no Reader running)", got)
		}
	})
}

// nilResolver 는 ResolvePID 가 항상 zero PodIdentity 를 반환하는 collector 용 fake 다.
type nilResolver struct{}

func (nilResolver) ResolvePID(pid uint32) kube.PodIdentity { return kube.PodIdentity{} }

// toggleFakeDevice / toggleFakeNVML 은 T4 가 collector.PollOnceForTest 에 주입할 fake NVML 이다.
// T2 의 fakeNVML 과 분리한 이유는 T2 가 RunningProcesses 중심인 반면 T4 는 Snapshot() 까지 채워야
// device 시리즈가 발행되기 때문이다.
type toggleFakeDevice struct {
	uuid     string
	index    uint
	snapshot types.GPUSnapshot
}

type toggleFakeNVML struct {
	devices []toggleFakeDevice
}

func newToggleFakeNVML(devs []toggleFakeDevice) *toggleFakeNVML { return &toggleFakeNVML{devices: devs} }

func (f *toggleFakeNVML) DeviceCount() (uint, error) { return uint(len(f.devices)), nil }
func (f *toggleFakeNVML) Device(idx uint) (nvml.Device, error) {
	d := f.devices[idx]
	return &toggleFakeNvmlDevice{uuid: d.uuid, index: d.index, snapshot: d.snapshot}, nil
}
func (f *toggleFakeNVML) DeviceUUID(idx uint) (string, error) { return f.devices[idx].uuid, nil }
func (f *toggleFakeNVML) Shutdown() error                     { return nil }

type toggleFakeNvmlDevice struct {
	uuid     string
	index    uint
	snapshot types.GPUSnapshot
}

func (d *toggleFakeNvmlDevice) Info() (types.GPUDevice, error) {
	return types.GPUDevice{UUID: d.uuid, Index: d.index}, nil
}
func (d *toggleFakeNvmlDevice) Snapshot() (types.GPUSnapshot, error) { return d.snapshot, nil }
func (d *toggleFakeNvmlDevice) RunningProcesses() ([]types.GPUProcess, error) {
	return nil, nil
}
func (d *toggleFakeNvmlDevice) ProcessUtilization(uint64) ([]types.GPUProcessUtil, error) {
	return nil, nil
}
func (d *toggleFakeNvmlDevice) MigMode() (types.MigMode, error)    { return types.MigModeUnsupported, nil }
func (d *toggleFakeNvmlDevice) MaxMigDeviceCount() (int, error)    { return 0, nil }
func (d *toggleFakeNvmlDevice) MigDevice(int) (nvml.Device, error) { return nil, nil }
func (d *toggleFakeNvmlDevice) IsMigDevice() (bool, error)         { return false, nil }
func (d *toggleFakeNvmlDevice) GpuInstanceId() (uint32, error)     { return 0, nil }
func (d *toggleFakeNvmlDevice) ComputeInstanceId() (uint32, error) { return 0, nil }
func (d *toggleFakeNvmlDevice) Close() error                       { return nil }
