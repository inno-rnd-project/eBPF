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
	"netobs/internal/gpuobs/mps"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// mpsDetect 는 mps.Detect 의 test seam 이다. 운영 코드는 본 변수의 기본값으로 mps.Detect 를 사용 하고,
// 단위 테스트는 분기 결정성을 위해 본 변수를 임시 함수로 교체한다.
var mpsDetect = mps.Detect

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
	devSet   *nvml.DeviceSet

	// recordSnapshot은 metrics.RecordPodSnapshot을 위한 test seam이다.
	// 운영 코드는 New에서 metrics.RecordPodSnapshot을 기본값으로 받고, 단위 테스트에서는
	// spy 함수로 교체해 호출 인자(snapshot)를 검증한다.
	recordSnapshot func(node string, samples []metrics.PodGPUSample)
	// recordMigSnapshot 은 #104 MIG 활성 시리즈 발행 test seam. 운영 기본값 metrics.RecordPodMigSnapshot.
	recordMigSnapshot func(node string, samples []metrics.PodMigGPUSample)
}

// New는 NVML 핸들과 Config, 그리고 선택적 PodResolver를 받아 Collector를 구성한다.
// nvml이 nil이거나 resolver가 nil이어도 생성은 성공한다. nvml nil은 device 폴링 자체를 비활성화하고,
// resolver nil은 device 폴링은 유지하되 per-pod 귀속 단계만 건너뛴다.
// resolver가 주입되더라도 cfg.PodMetricsEnabled가 false이면 RunningProcesses 호출 자체를 건너뛰어
// /proc/<pid>/cgroup 읽기 비용을 발생시키지 않는다.
func New(nv nvml.NVML, cfg config.Config, resolver PodResolver) *Collector {
	return &Collector{
		nvml:              nv,
		cfg:               cfg,
		resolver:          resolver,
		recordSnapshot:    metrics.RecordPodSnapshot,
		recordMigSnapshot: metrics.RecordPodMigSnapshot,
	}
}

// Run은 수집 루프를 실행한다.
//
//   - NVML 불가 또는 메트릭 비활성화 시: warn 로그 후 ctx.Done 까지 대기
//   - 정상 경로: nvml.DeviceSet 생성 → 매 cfg.GPUPollInterval 주기로 Sync (hot-add/remove 동적 반영) +
//     Snapshot 으로 현재 device 슬라이스 폴링
//   - ctx 취소 시 device handle 모두 Close + NVML Shutdown 까지 수행한 뒤 반환
//
// device hot-plug 는 nvml.DeviceSet 의 UUID 기반 차분 동기화로 흡수된다 (Phase 5, issue #34).
// 다만 GPU device 자체의 reset / driver reload 후 동일 UUID 재등장 회복은 별도 이슈로 분리한다.
func (c *Collector) Run(ctx context.Context, onReady func()) error {
	if c.nvml == nil {
		log.Printf("gpuobs collector disabled: NVML unavailable")
		signalReady(onReady)
		<-ctx.Done()
		return nil
	}

	// non-nil NVML 핸들을 받은 이상 lifecycle은 collector가 소유한다.
	// flag 기반 disable 경로에서도 defer가 먼저 등록되어 Shutdown이 보장된다.
	// DeviceSet.Close 는 NVML.Shutdown 직전에 수행해 GPM sample 등 device-scope 자원이 먼저 해제되도록 한다.
	c.devSet = nvml.NewDeviceSet(c.nvml)
	defer func() {
		if err := c.devSet.Close(); err != nil {
			log.Printf("nvml device set close: %v", err)
		}
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

	// 첫 sync 로 device 핸들 등록 후 1회 폴링 → readiness. sync 실패는 warn 로그만 남기고 빈 셋으로 진행한다
	// (이후 ticker 마다 재시도되어 자연 회복).
	if err := c.devSet.Sync(); err != nil {
		log.Printf("gpuobs: initial device sync: %v", err)
	}
	c.pollOnce()
	signalReady(onReady)

	t := time.NewTicker(c.cfg.GPUPollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := c.devSet.Sync(); err != nil {
				log.Printf("gpuobs: device sync: %v", err)
			}
			c.pollOnce()
		}
	}
}

// podGPUKey는 (Pod, GPU device) 단위 합산용 키다. Pod UID는 클러스터 내 유일하고 GPU UUID/Index는
// 노드 안에서 device를 구분하므로 본 키 조합은 single-node agent 한 폴 사이클 내에서 일의적이다.
type podGPUKey struct {
	podUID   string
	gpuUUID  string
	gpuIndex uint
}

// podMigKey 는 #104 MIG 활성 환경의 (Pod, MIG instance) 단위 합산 키다. parent gpuUUID 와 instance
// 식별자 (migUUID, gpuInstanceID) 조합 으로 동일 device 내 instance 간 분리 보장.
type podMigKey struct {
	podUID         string
	gpuUUID        string
	migUUID        string
	gpuInstanceID  uint32
}

// pollOnce는 DeviceSet.Snapshot 으로 현재 알려진 device 핸들을 받아 각 device 마다 Snapshot 과
// RunningProcesses 를 읽고, per-pod 분량은 (podUID, gpu) 키로 합산한 뒤 한 번에
// metrics.RecordPodSnapshot 으로 전달한다.
// 합산 단계가 있어 동일 Pod의 다중 GPU 프로세스가 라벨 충돌로 덮어써지는 문제가 사라지고,
// metrics 측의 diff cleanup이 종료된 Pod의 stale 시리즈를 자동 제거한다.
//
// 한 device에서 Snapshot/RunningProcesses 실패는 다른 device 폴링을 막지 않는다.
// per-pod 귀속은 resolver 주입 + cfg 토글이 모두 활성일 때만 시도되며, 그 외에는 device-level만 수행한다.
func (c *Collector) pollOnce() {
	perPodEnabled := c.resolver != nil && c.cfg.PodMetricsEnabled

	var aggregated map[podGPUKey]*metrics.PodGPUSample
	var aggregatedMig map[podMigKey]*metrics.PodMigGPUSample
	if perPodEnabled {
		aggregated = make(map[podGPUKey]*metrics.PodGPUSample)
		aggregatedMig = make(map[podMigKey]*metrics.PodMigGPUSample)
	}

	// #104 MPS daemon active 여부 는 노드 단위 신호 라 device loop 진입 전 1회 detect.
	mpsOn := mpsDetect()

	for _, dev := range c.devSet.Snapshot() {
		snap, err := dev.Snapshot()
		if err != nil {
			// wrapErr가 이미 idx 컨텍스트를 포함한다.
			log.Printf("gpuobs: snapshot: %v", err)
			continue
		}
		metrics.Record(c.cfg.NodeName, snap)

		// #104 self-health 메트릭 발행. MigMode 는 nvml init 단계 에서 캐싱 되어 매 poll 비용 zero.
		// MPS 는 위에서 1회 detect 한 결과 를 device 라벨 4종 으로 동일 값 발행.
		metrics.RecordMigMode(c.cfg.NodeName, snap.Device)
		metrics.RecordMpsActive(c.cfg.NodeName, snap.Device, mpsOn)

		if !perPodEnabled {
			continue
		}

		procs, err := dev.RunningProcesses()
		if err != nil {
			log.Printf("gpuobs: running processes: %v", err)
			continue
		}
		// #104 per-process SM util 수집. NVML 이 RunningProcesses (메모리 사용량) 와 ProcessUtilization
		// (SM util) 을 별도 호출로 노출 하므로 device 마다 두 호출 결과를 PID 기준 inner join 한다.
		// ProcessUtilization 실패는 비치명적 으로 흡수 한다 (메모리 메트릭은 계속 발행 되며 util 만 0 으로 강등).
		utils, err := dev.ProcessUtilization()
		if err != nil {
			log.Printf("gpuobs: process utilization: %v", err)
			utils = nil
		}
		utilByPID := buildProcessUtilMap(utils)
		for _, p := range procs {
			id := c.resolver.ResolvePID(p.PID)
			if !id.IsPod() {
				// unresolved / host process / 미동기화 Pod 등은 합산 키 생성도 건너뛴다.
				continue
			}
			key := podGPUKey{
				podUID:   id.PodUID,
				gpuUUID:  snap.Device.UUID,
				gpuIndex: snap.Device.Index,
			}
			smUtil := utilByPID[p.PID]
			if v, ok := aggregated[key]; ok {
				v.MemUsedBytes += p.MemoryUsedBytes
				v.SmUtilPct = capUtilPct(uint32(v.SmUtilPct) + smUtil)
				continue
			}
			aggregated[key] = &metrics.PodGPUSample{
				ID:           id,
				Device:       snap.Device,
				MemUsedBytes: p.MemoryUsedBytes,
				SmUtilPct:    smUtil,
			}
		}

		// #104 MIG 활성 경로. parent device 의 MigMode 가 Enabled 일 때만 instance enumerate 후
		// instance 별 process util 산정. RTX 3090 같은 MIG 미지원 device 에서는 본 분기 미진입 (graceful
		// degradation). instance enumerate / per-instance util 실패는 비치명 적 으로 흡수.
		if snap.Device.MigMode == types.MigModeEnabled {
			c.collectMigInstances(dev, snap.Device, aggregatedMig)
		}
	}

	// per-pod 토글이 켜진 경로에서만 RecordPodSnapshot을 호출한다.
	// 빈 aggregated여도 호출해야 직전 poll에 있던 라벨이 metrics 측 diff cleanup으로 삭제된다.
	if perPodEnabled {
		samples := make([]metrics.PodGPUSample, 0, len(aggregated))
		for _, v := range aggregated {
			samples = append(samples, *v)
		}
		c.recordSnapshot(c.cfg.NodeName, samples)

		// #104 MIG 활성 시리즈 도 동일 cleanup invariant 유지 위해 항상 호출. RTX 3090 같은 MIG 미지원
		// device 만 있는 노드 에서는 aggregatedMig 가 항상 empty 라 cleanup 외 부수 효과 zero.
		migSamples := make([]metrics.PodMigGPUSample, 0, len(aggregatedMig))
		for _, v := range aggregatedMig {
			migSamples = append(migSamples, *v)
		}
		c.recordMigSnapshot(c.cfg.NodeName, migSamples)
	}
}

// collectMigInstances 는 #104 MIG 활성 device 의 instance 별 process util 을 수집해 aggregated 맵 에
// 누적 한다. enumerate / per-instance 호출 실패는 비치명적 으로 흡수 (다른 instance 폴링 차단 하지 않음).
// instance handle 의 lifecycle 은 parent deviceImpl 이 캐시 슬롯으로 보유 하므로 본 함수는 Close 호출
// 책임 이 없다 (parent Close 시 children 일괄 해제). 캐싱 으로 instance 의 processUtilLastSeenTs 와
// unsupported 캐시 가 lifetime 동안 보존 되어 매 poll sample 중복 / NOT_SUPPORTED 반복 호출이 사라진다.
func (c *Collector) collectMigInstances(parent nvml.Device, parentDev types.GPUDevice, aggregated map[podMigKey]*metrics.PodMigGPUSample) {
	count, err := parent.MaxMigDeviceCount()
	if err != nil {
		log.Printf("gpuobs: mig max device count: %v", err)
		return
	}
	for i := 0; i < count; i++ {
		instance, err := parent.MigDevice(i)
		if err != nil {
			log.Printf("gpuobs: mig device idx=%d: %v", i, err)
			continue
		}
		if instance == nil {
			// 빈 슬롯 (instance 미생성 또는 disabled).
			continue
		}
		c.collectMigInstance(instance, parentDev, aggregated)
	}
}

// collectMigInstance 는 단일 MIG instance 의 process util 을 PID 단위 ResolvePID 후 (podUID, instance) 키
// 로 합산 한다. instance Info / GpuInstanceId 호출 실패는 비치명적 으로 흡수 하되 식별자 부재 시 다른
// instance 와 키 충돌 위험 이 있어 해당 instance 수집을 skip 한다.
func (c *Collector) collectMigInstance(instance nvml.Device, parentDev types.GPUDevice, aggregated map[podMigKey]*metrics.PodMigGPUSample) {
	info, err := instance.Info()
	if err != nil {
		log.Printf("gpuobs: mig instance info: %v", err)
		return
	}
	giID, err := instance.GpuInstanceId()
	if err != nil {
		// 식별자 부재 로 다른 instance 와 (podUID, mig_uuid, gi_id) 키 가 충돌 할 위험 회피 위해 skip.
		log.Printf("gpuobs: mig instance gpu instance id: %v", err)
		return
	}
	utils, err := instance.ProcessUtilization()
	if err != nil {
		log.Printf("gpuobs: mig instance process util: %v", err)
		return
	}
	for _, u := range utils {
		id := c.resolver.ResolvePID(u.PID)
		if !id.IsPod() {
			continue
		}
		key := podMigKey{
			podUID:        id.PodUID,
			gpuUUID:       parentDev.UUID,
			migUUID:       info.UUID,
			gpuInstanceID: giID,
		}
		if v, ok := aggregated[key]; ok {
			v.SmUtilPct = capUtilPct(uint32(v.SmUtilPct) + u.SmUtilPct)
			continue
		}
		aggregated[key] = &metrics.PodMigGPUSample{
			ID:            id,
			Device:        parentDev,
			MigUUID:       info.UUID,
			GpuInstanceID: giID,
			SmUtilPct:     u.SmUtilPct,
		}
	}
}

// signalReady는 onReady가 nil이어도 호출 가능하도록 감싼다.
func signalReady(onReady func()) {
	if onReady != nil {
		onReady()
	}
}

// buildProcessUtilMap 은 #104 ProcessUtilization 결과 슬라이스를 PID 기준 lookup map 으로 변환한다.
// NVML 이 짧은 sampling window 안에 같은 PID 의 sample 을 다회 반환할 수 있어 동일 PID 값은 max 채택
// (sampling jitter 보정) 한다. nil / empty 입력 은 빈 map 을 반환해 호출자 분기를 단순화한다.
func buildProcessUtilMap(utils []types.GPUProcessUtil) map[uint32]uint32 {
	if len(utils) == 0 {
		return map[uint32]uint32{}
	}
	out := make(map[uint32]uint32, len(utils))
	for _, u := range utils {
		if u.SmUtilPct > out[u.PID] {
			out[u.PID] = u.SmUtilPct
		}
	}
	return out
}

// capUtilPct 는 동일 GPU 의 multi-PID workload 합산 결과가 100 을 초과하지 않도록 cap 한다. NVML
// per-process SM util 은 process 단위 cost share 라 동일 GPU 의 모든 process 합이 100 을 초과하지
// 않아야 하지만, sampling jitter 와 multi-context 경합으로 일시 초과가 발생할 수 있어 안전 cap.
func capUtilPct(v uint32) uint32 {
	if v > 100 {
		return 100
	}
	return v
}
