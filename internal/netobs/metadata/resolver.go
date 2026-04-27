// Package metadata는 netobs eBPF Event를 enrichment하기 위한 캐시 계층을 제공한다.
// IP→PodIdentity 인덱스와 informer는 internal/kube로 승격되어 있고, 본 패키지는 그 위에
// netobs 고유의 socket-cookie flow 캐시와 cgroup/ifindex 런타임 hint 캐시를 얹는다.
// 두 캐시는 eBPF 이벤트 stage 흐름과 결합되어 있어 공용 패키지로 옮기지 않는다.
package metadata

import (
	"sync"
	"time"

	"netobs/internal/kube"
	"netobs/internal/netobs/drop"
	"netobs/internal/netobs/types"
)

type flowCacheEntry struct {
	Src kube.PodIdentity
	Dst kube.PodIdentity
}

type runtimeCacheEntry struct {
	ID       kube.PodIdentity
	LastSeen time.Time
}

// Resolver는 *kube.Resolver를 embed해 IP→PodIdentity 해석을 위임하고,
// 그 위에 netobs 고유의 flow / runtime hint 캐시와 Enrich 파이프라인을 보유한다.
// embed된 kube.Resolver의 mu는 IP 인덱스만 보호하며, 본 구조체의 mu는 flow/runtime 캐시를 보호한다.
// 두 mu가 분리되어 있어 IP 인덱스 갱신과 flow 캐시 lookup이 서로 블록되지 않는다.
type Resolver struct {
	*kube.Resolver

	mu sync.RWMutex

	// socket cookie flow cache (two-map generational)
	// 주기적으로 current → previous로 swap해 O(1) 만료를 수행한다.
	// lookup은 current 먼저, miss면 previous 확인 후 promote한다.
	// flowMaxCurrent를 두어 시간 기반 rotate 주기가 지나기 전이라도
	// current가 커지면 조기 rotate한다. 이로써 peak 메모리는
	// 2 × flowMaxCurrent × entry_size로 상한된다.
	flowCurrent     map[uint64]flowCacheEntry
	flowPrevious    map[uint64]flowCacheEntry
	flowRotateEvery time.Duration
	flowMaxCurrent  int
	lastFlowRotate  time.Time

	// runtime cache (cgroupid, ifindex -> pod identity)
	runtimeByCgroup   map[uint64]runtimeCacheEntry
	runtimeByIfindex  map[uint32]runtimeCacheEntry
	runtimeTTL        time.Duration
	runtimeSweepEvery time.Duration
	lastRuntimeSweep  time.Time
}

// NewResolver는 kube.Resolver를 구성한 뒤 netobs 전용 캐시를 함께 초기화한 metadata.Resolver를 반환한다.
// 본 생성자는 호환성을 위해 기존 시그니처(localNode, resyncPeriod)를 유지하지만,
// 호출부가 *kube.Resolver를 별도로 보유해야 한다면 다음 단계 refactor에서 명시 DI로 교체될 수 있다.
func NewResolver(localNode string, resyncPeriod time.Duration) *Resolver {
	return &Resolver{
		Resolver: kube.NewResolver(localNode, resyncPeriod),

		// socket cookie flow cache (two-map generational).
		// rotate 주기(2.5분)의 1~2배 범위에서 entry가 생존하므로
		// 기존 5분 TTL을 근사하면서 sweep O(N) 블록킹을 제거한다.
		// flowCacheEntry는 Src/Dst 각 PodIdentity (string 8개 필드) 구성으로 ~0.8~1KB 수준,
		// Go map 오버헤드 포함 시 100,000 × ~1KB × 2 (current+previous) 기준 peak ≈ ~200MB.
		flowCurrent:     make(map[uint64]flowCacheEntry),
		flowPrevious:    make(map[uint64]flowCacheEntry),
		flowRotateEvery: 2*time.Minute + 30*time.Second,
		flowMaxCurrent:  100_000,
		lastFlowRotate:  time.Now(),

		// runtime
		runtimeByCgroup:   make(map[uint64]runtimeCacheEntry),
		runtimeByIfindex:  make(map[uint32]runtimeCacheEntry),
		runtimeTTL:        2 * time.Minute,
		runtimeSweepEvery: 30 * time.Second,
	}
}

// lookupFlow는 current 맵을 먼저 확인하고 miss면 previous 맵을 확인한다.
// previous hit 시 해당 entry를 current로 promote해 다음 rotate에서
// 만료되지 않도록 한다. promote를 위해 read lock을 write lock으로 승격한다.
func (r *Resolver) lookupFlow(cookie uint64) (flowCacheEntry, bool) {
	if cookie == 0 {
		return flowCacheEntry{}, false
	}

	r.mu.RLock()
	if entry, ok := r.flowCurrent[cookie]; ok {
		r.mu.RUnlock()
		return entry, true
	}
	entry, ok := r.flowPrevious[cookie]
	r.mu.RUnlock()

	if !ok {
		return flowCacheEntry{}, false
	}

	// previous hit → current로 promote.
	// RUnlock과 Lock 사이에 다른 goroutine이 먼저 promote했을 수 있으므로
	// current에 이미 있다면 건너뛴다.
	r.mu.Lock()
	if _, already := r.flowCurrent[cookie]; !already {
		r.flowCurrent[cookie] = entry
	}
	r.mu.Unlock()

	return entry, true
}

// maybeRotateFlowsLocked는 rotate 조건이 되면 current를 previous로 밀어내고
// 새 current 맵을 만든다. 기존 O(N) sweep 순회를 O(1) 포인터 교체로 대체한다.
//
// rotate는 두 조건 중 하나만 만족해도 일어난다:
//  1. 시간 기반: 마지막 rotate로부터 flowRotateEvery 경과
//  2. 크기 기반: current 크기가 flowMaxCurrent 초과
//
// 크기 기반 조기 rotate로 arrival rate 급증 시에도 peak 메모리가
// 2 × flowMaxCurrent × entry_size로 상한된다.
func (r *Resolver) maybeRotateFlowsLocked(now time.Time) {
	timeUp := now.Sub(r.lastFlowRotate) >= r.flowRotateEvery
	sizeUp := len(r.flowCurrent) >= r.flowMaxCurrent
	if !timeUp && !sizeUp {
		return
	}

	r.flowPrevious = r.flowCurrent
	r.flowCurrent = make(map[uint64]flowCacheEntry)
	r.lastFlowRotate = now
}

func (r *Resolver) rememberFlow(cookie uint64, src, dst kube.PodIdentity, now time.Time) {
	if cookie == 0 {
		return
	}
	if !src.Known() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.maybeRotateFlowsLocked(now)
	r.flowCurrent[cookie] = flowCacheEntry{
		Src: src,
		Dst: dst,
	}
}

func (r *Resolver) maybeSweepRuntimeLocked(now time.Time) {
	if !r.lastRuntimeSweep.IsZero() && now.Sub(r.lastRuntimeSweep) < r.runtimeSweepEvery {
		return
	}

	cutoff := now.Add(-r.runtimeTTL)

	for k, v := range r.runtimeByCgroup {
		if v.LastSeen.Before(cutoff) {
			delete(r.runtimeByCgroup, k)
		}
	}
	for k, v := range r.runtimeByIfindex {
		if v.LastSeen.Before(cutoff) {
			delete(r.runtimeByIfindex, k)
		}
	}

	r.lastRuntimeSweep = now
}

func (r *Resolver) lookupCgroupHint(cgroupID uint64, now time.Time) (kube.PodIdentity, bool) {
	if cgroupID == 0 {
		return kube.PodIdentity{}, false
	}

	r.mu.RLock()
	entry, ok := r.runtimeByCgroup[cgroupID]
	r.mu.RUnlock()

	if !ok || now.Sub(entry.LastSeen) > r.runtimeTTL {
		return kube.PodIdentity{}, false
	}
	return entry.ID, true
}

func (r *Resolver) lookupIfindexHint(ifindex uint32, now time.Time) (kube.PodIdentity, bool) {
	if ifindex == 0 {
		return kube.PodIdentity{}, false
	}

	r.mu.RLock()
	entry, ok := r.runtimeByIfindex[ifindex]
	r.mu.RUnlock()

	if !ok || now.Sub(entry.LastSeen) > r.runtimeTTL {
		return kube.PodIdentity{}, false
	}
	return entry.ID, true
}

func (r *Resolver) rememberCgroupHint(cgroupID uint64, id kube.PodIdentity, now time.Time) {
	if cgroupID == 0 || !id.IsPod() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.maybeSweepRuntimeLocked(now)
	r.runtimeByCgroup[cgroupID] = runtimeCacheEntry{
		ID:       id,
		LastSeen: now,
	}
}

func (r *Resolver) rememberIfindexHint(ifindex uint32, id kube.PodIdentity, now time.Time) {
	if ifindex == 0 || !id.IsPod() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.maybeSweepRuntimeLocked(now)
	r.runtimeByIfindex[ifindex] = runtimeCacheEntry{
		ID:       id,
		LastSeen: now,
	}
}

func (r *Resolver) applyRuntimeHints(ev types.Event, srcIP, dstIP string, src, dst kube.PodIdentity, now time.Time) (kube.PodIdentity, kube.PodIdentity) {
	if !src.IsPod() {
		if id, ok := r.lookupCgroupHint(ev.CgroupID, now); ok {
			src = kube.StrongerIdentity(src, kube.WithObservedIP(id, srcIP))
		}
	}
	if !src.IsPod() && ev.Ifindex != 0 {
		if id, ok := r.lookupIfindexHint(ev.Ifindex, now); ok {
			src = kube.StrongerIdentity(src, kube.WithObservedIP(id, srcIP))
		}
	}
	if !dst.IsPod() && ev.SkbIif != 0 {
		if id, ok := r.lookupIfindexHint(ev.SkbIif, now); ok {
			dst = kube.StrongerIdentity(dst, kube.WithObservedIP(id, dstIP))
		}
	}
	return src, dst
}

func (r *Resolver) rememberRuntimeHints(ev types.Event, src, dst kube.PodIdentity, now time.Time) {
	switch ev.Stage {
	case types.StageSendmsgRet:
		if src.IsPod() {
			r.rememberCgroupHint(ev.CgroupID, src, now)
		}

	case types.StageToVeth, types.StageToDevQ:
		if src.IsPod() {
			r.rememberCgroupHint(ev.CgroupID, src, now)
			r.rememberIfindexHint(ev.Ifindex, src, now)
		}

	case types.StageRetrans, types.StageDrop:
		if src.IsPod() {
			r.rememberIfindexHint(ev.Ifindex, src, now)
		}
	}

	if dst.IsPod() && ev.SkbIif != 0 {
		r.rememberIfindexHint(ev.SkbIif, dst, now)
	}
}

func deriveTrafficScope(src, dst kube.PodIdentity) string {
	switch {
	case src.IsPod() && dst.IsPod():
		if src.NodeName != "" && dst.NodeName != "" {
			if src.NodeName == dst.NodeName {
				return "same_node"
			}
			return "cross_node"
		}
		return "pod_to_pod"

	case src.IsPod() && dst.IsService():
		return "to_service"

	case src.IsService() && dst.IsPod():
		return "from_service"

	case src.IsPod() && dst.IsExternal():
		return "to_external"

	case src.IsExternal() && dst.IsPod():
		return "from_external"

	case src.IsPod() && dst.IsNode():
		if src.NodeName != "" && src.NodeName == dst.NodeName {
			return "to_host_local"
		}
		return "to_node"

	case src.IsNode() && dst.IsPod():
		if src.NodeName != "" && src.NodeName == dst.NodeName {
			return "from_host_local"
		}
		return "from_node"

	case src.IsNode() && dst.IsNode():
		if src.NodeName != "" && src.NodeName == dst.NodeName {
			return "host_local"
		}
		return "node_to_node"

	case src.IsService() && dst.IsExternal():
		return "service_to_external"

	case src.IsExternal() && dst.IsService():
		return "external_to_service"

	case src.IsPod() && dst.IsUnresolved():
		return "to_unresolved"

	case src.IsUnresolved() && dst.IsPod():
		return "from_unresolved"

	case src.IsService() && dst.IsUnresolved():
		return "service_to_unresolved"

	case src.IsUnresolved() && dst.IsService():
		return "unresolved_to_service"

	default:
		return "unresolved"
	}
}

// Enrich는 raw eBPF Event를 EnrichedEvent로 보강한다.
// IP 해석은 embed된 kube.Resolver에 위임하고, 그 결과에 socket-cookie flow 캐시 hit과
// cgroup/ifindex runtime hint를 합쳐 양 끝의 식별을 가능한 강하게 만든다.
func (r *Resolver) Enrich(ev types.Event, mapper *drop.Mapper) types.EnrichedEvent {
	srcIP := types.U32ToIPv4(ev.Saddr)
	dstIP := types.U32ToIPv4(ev.Daddr)

	now := time.Now()

	src := r.ResolveIP(srcIP)
	dst := r.ResolveIP(dstIP)

	if cached, ok := r.lookupFlow(ev.SocketCookie); ok {
		src = kube.StrongerIdentity(src, kube.WithObservedIP(cached.Src, srcIP))
		dst = kube.StrongerIdentity(dst, kube.WithObservedIP(cached.Dst, dstIP))
	}

	src, dst = r.applyRuntimeHints(ev, srcIP, dstIP, src, dst, now)

	if src.Known() {
		switch ev.Stage {
		case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ, types.StageRetrans, types.StageDrop:
			r.rememberFlow(ev.SocketCookie, src, dst, now)
		}
	}

	r.rememberRuntimeHints(ev, src, dst, now)

	reasonName := ""
	reasonCategory := ""
	if ev.Stage == types.StageDrop && mapper != nil {
		reasonName, reasonCategory = mapper.Describe(ev.Reason)
	}

	return types.EnrichedEvent{
		Raw:            ev,
		Stage:          types.StageName(ev.Stage),
		CommText:       types.CommString(ev.Comm),
		Direction:      "egress",
		TrafficScope:   deriveTrafficScope(src, dst),
		ObservedNode:   r.LocalNode(),
		SrcIPText:      srcIP,
		DstIPText:      dstIP,
		Src:            src,
		Dst:            dst,
		DropReasonName: reasonName,
		DropCategory:   reasonCategory,
	}
}
