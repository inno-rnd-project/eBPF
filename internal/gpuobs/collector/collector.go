// Package collector는 gpuobs의 NVML 폴링 루프를 소유한다.
// NVML 핸들이 없거나 메트릭이 비활성화된 경우 수집을 건너뛰고 ctx 종료까지 대기해,
// non-GPU 노드에서도 바이너리와 health/ready 응답이 유지되도록 한다.
package collector

import (
	"context"
	"log"
	"time"

	"netobs/internal/gpuobs/config"
	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/kube"
)

// PodResolver는 collector가 PID → PodIdentity 해석을 위해 의존하는 최소 인터페이스다.
// 운영에서는 *kube.Resolver가 자연스럽게 만족하며, 단위 테스트에서는 fake로 주입한다.
type PodResolver interface {
	ResolvePID(pid uint32) kube.PodIdentity
}

// Collector는 NVML 폴링 루프를 소유한다.
type Collector struct {
	nvml     nvml.NVML
	cfg      config.Config
	resolver PodResolver
	devices  []nvml.Device
}

// New는 NVML 핸들과 Config, 그리고 선택적 PodResolver를 받아 Collector를 구성한다.
// nvml이 nil이거나 resolver가 nil이어도 생성은 성공한다. nvml nil은 device 폴링 자체를 비활성화하고,
// resolver nil은 device 폴링은 유지하되 per-pod 귀속 단계만 건너뛴다.
// resolver가 주입되더라도 cfg.PodMetricsEnabled가 false이면 ResolvePID 호출 자체를 건너뛰어
// /proc/<pid>/cgroup 읽기 비용을 발생시키지 않는다.
func New(nv nvml.NVML, cfg config.Config, resolver PodResolver) *Collector {
	return &Collector{nvml: nv, cfg: cfg, resolver: resolver}
}

// Run은 수집 루프를 실행한다.
//
//   - NVML 불가 또는 메트릭 비활성화 시: warn 로그 후 ctx.Done 까지 대기
//   - 정상 경로: Run 시작 시 device 핸들을 1회 discover → 초기 pollOnce → readiness 신호 →
//     cfg.GPUPollInterval 주기로 캐시된 device 슬라이스만 반복 폴링
//   - ctx 취소 시 NVML Shutdown까지 수행한 뒤 반환
//
// device hot-plug는 Phase 2 비목표이며, discover 이후 추가된 device는 다음 기동까지 인식되지 않는다.
func (c *Collector) Run(ctx context.Context, onReady func()) error {
	if c.nvml == nil {
		log.Printf("gpuobs collector disabled: NVML unavailable")
		signalReady(onReady)
		<-ctx.Done()
		return nil
	}

	// non-nil NVML 핸들을 받은 이상 lifecycle은 collector가 소유한다.
	// flag 기반 disable 경로에서도 defer가 먼저 등록되어 Shutdown이 보장된다.
	defer func() {
		if err := c.nvml.Shutdown(); err != nil {
			log.Printf("nvml shutdown: %v", err)
		}
	}()

	if !c.cfg.GPUMetricsEnabled {
		log.Printf("gpuobs collector disabled: GPU_METRICS_ENABLED=false")
		signalReady(onReady)
		<-ctx.Done()
		return nil
	}

	// device 핸들과 정적 속성을 1회 discover해 이후 폴링에서 재조회가 일어나지 않도록 한다.
	c.devices = c.discover()

	// 초기 1회 폴링으로 기동 직후 /metrics를 채운 뒤 readiness를 올린다.
	c.pollOnce()
	signalReady(onReady)

	t := time.NewTicker(c.cfg.GPUPollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.pollOnce()
		}
	}
}

// discover는 Run 진입 시 1회 실행되어 NVML device 개수를 읽고 각 index의 핸들을 얻는다.
// 개별 device의 핸들 획득이 실패하면 해당 device만 건너뛰고 계속 진행한다.
func (c *Collector) discover() []nvml.Device {
	count, err := c.nvml.DeviceCount()
	if err != nil {
		log.Printf("gpuobs: device count: %v", err)
		return nil
	}
	devices := make([]nvml.Device, 0, count)
	for i := uint(0); i < count; i++ {
		dev, err := c.nvml.Device(i)
		if err != nil {
			log.Printf("gpuobs: device idx=%d: %v", i, err)
			continue
		}
		devices = append(devices, dev)
	}
	return devices
}

// pollOnce는 캐시된 device 핸들마다 Snapshot과 RunningProcesses를 읽어 metrics로 전달한다.
// 한 device에서 실패해도 나머지 device 폴링은 계속한다. per-pod 귀속은 resolver 주입 + cfg
// 토글이 모두 활성화된 경우에만 시도하며, 그 외에는 device-level 폴링만 수행한다.
func (c *Collector) pollOnce() {
	perPodEnabled := c.resolver != nil && c.cfg.PodMetricsEnabled

	for _, dev := range c.devices {
		snap, err := dev.Snapshot()
		if err != nil {
			// wrapErr가 이미 idx 컨텍스트를 포함한다.
			log.Printf("gpuobs: snapshot: %v", err)
			continue
		}
		metrics.Record(c.cfg.NodeName, snap)

		if !perPodEnabled {
			continue
		}

		procs, err := dev.RunningProcesses()
		if err != nil {
			log.Printf("gpuobs: running processes: %v", err)
			continue
		}
		for _, p := range procs {
			id := c.resolver.ResolvePID(p.PID)
			// RecordPod 내부에서 IsPod()와 podMetricsEnabled를 다시 검사해 unresolved/host PID는 무시된다.
			metrics.RecordPod(c.cfg.NodeName, snap.Device, id, p.MemoryUsedBytes)
		}
	}
}

// signalReady는 onReady가 nil이어도 호출 가능하도록 감싼다.
func signalReady(onReady func()) {
	if onReady != nil {
		onReady()
	}
}
