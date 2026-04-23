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
	nvml nvml.NVML
	cfg  config.Config
}

// New는 NVML 핸들과 Config를 받아 Collector를 구성한다.
// nvml이 nil이어도 생성은 성공하며, Run 시점에 graceful disable 경로로 분기된다.
func New(nv nvml.NVML, cfg config.Config) *Collector {
	return &Collector{nvml: nv, cfg: cfg}
}

// Run은 수집 루프를 실행한다.
//
//   - NVML 불가 또는 메트릭 비활성화 시: warn 로그 후 ctx.Done 까지 대기
//   - 정상 경로: 최초 1회 pollOnce → readiness 신호 → cfg.GPUPollInterval 주기로 폴링
//   - ctx 취소 시 NVML Shutdown까지 수행한 뒤 반환
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

// pollOnce는 현재 시점의 모든 device 스냅샷을 읽어 metrics로 전달한다.
// 한 device에서 실패해도 나머지 device 폴링은 계속한다.
func (c *Collector) pollOnce() {
	count, err := c.nvml.DeviceCount()
	if err != nil {
		log.Printf("gpuobs: device count: %v", err)
		return
	}
	for i := uint(0); i < count; i++ {
		dev, err := c.nvml.Device(i)
		if err != nil {
			log.Printf("gpuobs: device idx=%d: %v", i, err)
			continue
		}
		snap, err := dev.Snapshot()
		if err != nil {
			log.Printf("gpuobs: snapshot idx=%d: %v", i, err)
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
