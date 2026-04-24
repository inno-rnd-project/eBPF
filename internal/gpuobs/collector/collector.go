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
)

// Collector는 NVML 폴링 루프를 소유한다.
type Collector struct {
	nvml    nvml.NVML
	cfg     config.Config
	devices []nvml.Device
}

// New는 NVML 핸들과 Config를 받아 Collector를 구성한다.
// nvml이 nil이어도 생성은 성공하며, Run 시점에 graceful disable 경로로 분기된다.
func New(nv nvml.NVML, cfg config.Config) *Collector {
	return &Collector{nvml: nv, cfg: cfg}
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

// pollOnce는 캐시된 device 핸들마다 Snapshot을 읽어 metrics로 전달한다.
// 한 device에서 실패해도 나머지 device 폴링은 계속한다.
func (c *Collector) pollOnce() {
	for _, dev := range c.devices {
		snap, err := dev.Snapshot()
		if err != nil {
			// wrapErr가 이미 idx 컨텍스트를 포함한다.
			log.Printf("gpuobs: snapshot: %v", err)
			continue
		}
		metrics.Record(c.cfg.NodeName, snap)
	}
}

// signalReady는 onReady가 nil이어도 호출 가능하도록 감싼다.
func signalReady(onReady func()) {
	if onReady != nil {
		onReady()
	}
}
