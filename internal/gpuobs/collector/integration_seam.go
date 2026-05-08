//go:build integration

package collector

import (
	"netobs/internal/gpuobs/nvml"
)

// PollOnceForTest 는 통합 테스트가 ticker 없이 한 사이클의 polling 을 직접 호출할 수 있게 노출한다.
// Collector.Run 의 lifecycle 진입 (NVML Init / DeviceSet 생성 / readiness 신호) 을 모두 건너뛰고,
// 호출자가 미리 구성한 DeviceSet 을 그대로 주입한다.
func (c *Collector) PollOnceForTest(devSet *nvml.DeviceSet) {
	prev := c.devSet
	c.devSet = devSet
	defer func() { c.devSet = prev }()
	c.pollOnce()
}
