package collector

import (
	"net/http"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/gpuobs/types"
)

// #281 agent 로컬 실행 프로세스 조회 endpoint. PID 라벨은 카디널리티 때문에 메트릭으로 노출할 수
// 없어, 직전 poll 의 RunningProcesses 스냅샷을 Prometheus 를 거치지 않는 REST 경로로 돌려준다.
// 스냅샷 재사용이라 NVML 추가 호출이 없고, correlation-exporter 의 gpu-processes 프록시가 단일
// 진입점으로 노출한다.

// ProcessSnapshot 은 직전 poll 의 실행 프로세스 목록과 수집 시각을 돌려준다. per-pod 수집 토글이
// 꺼져 있으면 RunningProcesses 스윕 자체가 없어 빈 목록과 zero time 이다.
func (c *Collector) ProcessSnapshot() ([]types.GPUProcessDetail, time.Time) {
	c.procMu.RLock()
	defer c.procMu.RUnlock()
	return c.procList, c.procAt
}

// ProcessesHandler 는 GET /processes 핸들러를 만든다. 응답 계약은 types.GPUProcessListing 이다.
func (c *Collector) ProcessesHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		procs, at := c.ProcessSnapshot()
		if procs == nil {
			procs = []types.GPUProcessDetail{}
		}
		listing := types.GPUProcessListing{Node: c.cfg.NodeName, Processes: procs}
		if !at.IsZero() {
			listing.CollectedAt = at.Format(time.RFC3339)
		}
		apicommon.WriteJSON(w, listing)
	}
}
