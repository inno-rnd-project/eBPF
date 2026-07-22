// Package metadata는 netobs eBPF Event를 EnrichedEvent로 보강하는 캐시 계층을 제공한다.
// IP→PodIdentity 인덱스와 informer는 internal/kube로 승격되어 있고, 본 패키지는 그 위에
// netobs 고유의 socket-cookie flow 캐시와 cgroup/ifindex 런타임 hint 캐시를 얹어 Enrich 파이프라인을 구성한다.
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

// Enricher는 *kube.Resolver를 명시 DI로 받아 IP→PodIdentity 해석을 위임하고,
// 그 위에 netobs 고유의 flow / runtime hint 캐시와 Enrich 파이프라인을 보유한다.
// kube.Resolver의 lock과 본 구조체의 mu는 분리되어 IP 인덱스 갱신과 flow 캐시 lookup이 서로 블록되지 않는다.
type Enricher struct {
	kr *kube.Resolver

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

	// cgroupScanner 는 #228 의 cgroup id 역매핑 폴백이다. ringbuf 힌트가 학습되지 않은 (TCP 트래픽
	// 없는 UDP 전용) pod 의 cgroup 귀속을 informer 기반 inode 스캔 테이블로 해석한다. nil 이면 기존
	// 동작 그대로 힌트 캐시만 쓴다.
	cgroupScanner *CgroupScanner
}

// SetCgroupScanner 는 #228 폴백 스캐너를 주입한다. startup 시 1회 호출한다.
func (e *Enricher) SetCgroupScanner(c *CgroupScanner) {
	e.cgroupScanner = c
}

// NewEnricher는 외부에서 구성된 *kube.Resolver를 받아 netobs 전용 캐시와 함께 Enricher를 구성한다.
// IP→PodIdentity 인덱스의 lifecycle(Start/HasSynced)은 호출자가 kube.Resolver 측에서 관리한다.
func NewEnricher(kr *kube.Resolver) *Enricher {
	return &Enricher{
		kr: kr,

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
func (e *Enricher) lookupFlow(cookie uint64) (flowCacheEntry, bool) {
	if cookie == 0 {
		return flowCacheEntry{}, false
	}

	e.mu.RLock()
	if entry, ok := e.flowCurrent[cookie]; ok {
		e.mu.RUnlock()
		return entry, true
	}
	entry, ok := e.flowPrevious[cookie]
	e.mu.RUnlock()

	if !ok {
		return flowCacheEntry{}, false
	}

	// previous hit → current로 promote.
	// RUnlock과 Lock 사이에 다른 goroutine이 먼저 promote했을 수 있으므로
	// current에 이미 있다면 건너뛴다.
	e.mu.Lock()
	if _, already := e.flowCurrent[cookie]; !already {
		e.flowCurrent[cookie] = entry
	}
	e.mu.Unlock()

	return entry, true
}

// maybeRotateFlowsLocked는 rotate 조건이 되면 current를 previous로 밀어내고
// 새 current 맵을 만든다. 기존 O(N) sweep 순회를 O(1) 포인터 교체로 대체한다.
//
// rotate는 두 조건 중 하나만 만족해도 일어난다:
//  1. 시간 기반: 마지막 rotate로부터 flowRotateEvery 경과
//  2. 크기 기반: current 크기가 flowMaxCurrent 이상
//
// 크기 기반 조기 rotate로 arrival rate 급증 시에도 peak 메모리가
// 2 × flowMaxCurrent × entry_size로 상한된다.
func (e *Enricher) maybeRotateFlowsLocked(now time.Time) {
	timeUp := now.Sub(e.lastFlowRotate) >= e.flowRotateEvery
	sizeUp := len(e.flowCurrent) >= e.flowMaxCurrent
	if !timeUp && !sizeUp {
		return
	}

	e.flowPrevious = e.flowCurrent
	e.flowCurrent = make(map[uint64]flowCacheEntry)
	e.lastFlowRotate = now
}

func (e *Enricher) rememberFlow(cookie uint64, src, dst kube.PodIdentity, now time.Time) {
	if cookie == 0 {
		return
	}
	if !src.Known() {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.maybeRotateFlowsLocked(now)
	e.flowCurrent[cookie] = flowCacheEntry{
		Src: src,
		Dst: dst,
	}
}

func (e *Enricher) maybeSweepRuntimeLocked(now time.Time) {
	if !e.lastRuntimeSweep.IsZero() && now.Sub(e.lastRuntimeSweep) < e.runtimeSweepEvery {
		return
	}

	cutoff := now.Add(-e.runtimeTTL)

	for k, v := range e.runtimeByCgroup {
		if v.LastSeen.Before(cutoff) {
			delete(e.runtimeByCgroup, k)
		}
	}
	for k, v := range e.runtimeByIfindex {
		if v.LastSeen.Before(cutoff) {
			delete(e.runtimeByIfindex, k)
		}
	}

	e.lastRuntimeSweep = now
}

// ResolveCgroup은 외부 컴포넌트 (예: podbytes collector) 가 BPF map의 cgroup_id 키를 Pod 정체성으로
// 풀어쓸 때 사용하는 thread-safe lookup이다. 내부적으로는 lookupCgroupHint를 호출해 동일 runtime 캐시를
// 공유하므로 event 흐름으로 학습된 cgroup_id 매핑이 즉시 활용된다. 캐시 miss는 (zero, false) 반환이며
// 호출자가 다음 scrape에서 자연 재시도하는 패턴을 가정한다.
func (e *Enricher) ResolveCgroup(cgroupID uint64) (kube.PodIdentity, bool) {
	return e.lookupCgroupHint(cgroupID, time.Now())
}

func (e *Enricher) lookupCgroupHint(cgroupID uint64, now time.Time) (kube.PodIdentity, bool) {
	if cgroupID == 0 {
		return kube.PodIdentity{}, false
	}

	e.mu.RLock()
	entry, ok := e.runtimeByCgroup[cgroupID]
	e.mu.RUnlock()

	if ok && now.Sub(entry.LastSeen) <= e.runtimeTTL {
		return entry.ID, true
	}
	// #228 힌트 미학습 (TCP 트래픽 없는 UDP 전용 pod) 폴백. 스캐너 테이블은 informer 와 host cgroup
	// inode 스캔으로 주기 재구성되어 ringbuf 이벤트 없이도 귀속이 성립한다.
	if e.cgroupScanner != nil {
		if id, ok := e.cgroupScanner.Lookup(cgroupID); ok {
			return id, true
		}
	}
	return kube.PodIdentity{}, false
}

func (e *Enricher) lookupIfindexHint(ifindex uint32, now time.Time) (kube.PodIdentity, bool) {
	if ifindex == 0 {
		return kube.PodIdentity{}, false
	}

	e.mu.RLock()
	entry, ok := e.runtimeByIfindex[ifindex]
	e.mu.RUnlock()

	if !ok || now.Sub(entry.LastSeen) > e.runtimeTTL {
		return kube.PodIdentity{}, false
	}
	return entry.ID, true
}

func (e *Enricher) rememberCgroupHint(cgroupID uint64, id kube.PodIdentity, now time.Time) {
	if cgroupID == 0 || !id.IsPod() {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.maybeSweepRuntimeLocked(now)
	e.runtimeByCgroup[cgroupID] = runtimeCacheEntry{
		ID:       id,
		LastSeen: now,
	}
}

func (e *Enricher) rememberIfindexHint(ifindex uint32, id kube.PodIdentity, now time.Time) {
	// hostNetwork pod 의 이벤트에 실리는 ifindex 는 전용 veth 가 아니라 노드 전체가 공유하는 host
	// 인터페이스라, 힌트로 학습하면 kubelet 등 host 프로세스 트래픽까지 이 pod 로 오귀속된다 (#321).
	// cgroup 힌트는 pod 고유 식별이라 그대로 학습한다.
	if ifindex == 0 || !id.IsPod() || id.HostNetwork {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.maybeSweepRuntimeLocked(now)
	e.runtimeByIfindex[ifindex] = runtimeCacheEntry{
		ID:       id,
		LastSeen: now,
	}
}

func (e *Enricher) applyRuntimeHints(ev types.Event, srcIP, dstIP string, src, dst kube.PodIdentity, now time.Time) (kube.PodIdentity, kube.PodIdentity) {
	// cgroup_id 와 Ifindex 는 event 가 관측된 Pod 본인의 식별 정보다. send path 에서는 그 Pod 가
	// 흐름의 src (송신자) 이고, #65 의 receive path 에서는 BPF 측 src/dst swap 으로 dst (수신자) 가
	// 된다. 따라서 stage direction 으로 분기해 힌트를 올바른 쪽 endpoint 에 적재해야 외부 peer 가
	// 송신자인 ingress event 에서 src 가 수신 Pod 로 잘못 덮여써지는 회귀를 피할 수 있다.
	if types.StageDirection(ev.Stage) == "ingress" {
		if !dst.IsPod() {
			if id, ok := e.lookupCgroupHint(ev.CgroupID, now); ok {
				dst = kube.StrongerIdentity(dst, kube.WithObservedIP(id, dstIP))
			}
		}
		if !dst.IsPod() && ev.Ifindex != 0 {
			if id, ok := e.lookupIfindexHint(ev.Ifindex, now); ok {
				dst = kube.StrongerIdentity(dst, kube.WithObservedIP(id, dstIP))
			}
		}
	} else {
		if !src.IsPod() {
			if id, ok := e.lookupCgroupHint(ev.CgroupID, now); ok {
				src = kube.StrongerIdentity(src, kube.WithObservedIP(id, srcIP))
			}
		}
		if !src.IsPod() && ev.Ifindex != 0 {
			if id, ok := e.lookupIfindexHint(ev.Ifindex, now); ok {
				src = kube.StrongerIdentity(src, kube.WithObservedIP(id, srcIP))
			}
		}
	}
	// SkbIif 는 skb 의 inbound device ifindex 라 ingress 에서 의미가 있고 egress 에서는 보통 0 이다.
	// 두 케이스 모두 dst 측 hint 로만 쓰이며 direction 분기와 무관하게 동작이 일관된다.
	if !dst.IsPod() && ev.SkbIif != 0 {
		if id, ok := e.lookupIfindexHint(ev.SkbIif, now); ok {
			dst = kube.StrongerIdentity(dst, kube.WithObservedIP(id, dstIP))
		}
	}
	return src, dst
}

func (e *Enricher) rememberRuntimeHints(ev types.Event, src, dst kube.PodIdentity, now time.Time) {
	switch ev.Stage {
	case types.StageSendmsgRet:
		if src.IsPod() {
			e.rememberCgroupHint(ev.CgroupID, src, now)
		}

	case types.StageToVeth, types.StageToDevQ:
		if src.IsPod() {
			e.rememberCgroupHint(ev.CgroupID, src, now)
			e.rememberIfindexHint(ev.Ifindex, src, now)
		}

	case types.StageRetrans, types.StageDrop:
		if src.IsPod() {
			e.rememberIfindexHint(ev.Ifindex, src, now)
		}

	case types.StageRcvDemux, types.StageRcvEstablished, types.StageRcvApp:
		// #65 receive path 의 ingress event 는 흐름 방향이 swap 되어 dst 가 수신 Pod 다. cgroup_id /
		// skb_iif 는 모두 수신 Pod 의 식별 정보라 dst 측 캐시에 적재한다. tcp_v4_rcv (StageRcvL3) 는
		// sock 이 없어 emit 자체가 보류된 상태라 본 분기에는 포함하지 않는다.
		if dst.IsPod() {
			e.rememberCgroupHint(ev.CgroupID, dst, now)
		}
	}

	if dst.IsPod() && ev.SkbIif != 0 {
		e.rememberIfindexHint(ev.SkbIif, dst, now)
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
// IP 해석은 주입된 *kube.Resolver에 위임하고, 그 결과에 socket-cookie flow 캐시 hit과
// cgroup/ifindex runtime hint를 합쳐 양 끝의 식별을 가능한 강하게 만든다.
func (e *Enricher) Enrich(ev types.Event, mapper *drop.Mapper) types.EnrichedEvent {
	srcIP := types.IPToString(ev.Family, ev.Saddr)
	dstIP := types.IPToString(ev.Family, ev.Daddr)

	now := time.Now()

	src := e.kr.ResolveIP(srcIP)
	dst := e.kr.ResolveIP(dstIP)

	if cached, ok := e.lookupFlow(ev.SocketCookie); ok {
		src = kube.StrongerIdentity(src, kube.WithObservedIP(cached.Src, srcIP))
		dst = kube.StrongerIdentity(dst, kube.WithObservedIP(cached.Dst, dstIP))
	}

	src, dst = e.applyRuntimeHints(ev, srcIP, dstIP, src, dst, now)

	if src.Known() {
		switch ev.Stage {
		case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ, types.StageRetrans, types.StageDrop,
			types.StageRcvDemux, types.StageRcvEstablished, types.StageRcvApp:
			e.rememberFlow(ev.SocketCookie, src, dst, now)
		}
	}

	e.rememberRuntimeHints(ev, src, dst, now)

	reasonName := ""
	reasonCategory := ""
	reasonStage := ""
	if ev.Stage == types.StageDrop && mapper != nil {
		reasonName, reasonCategory = mapper.Describe(ev.Reason)
		reasonStage = mapper.Stage(reasonName)
	}

	return types.EnrichedEvent{
		Raw:            ev,
		Stage:          types.StageName(ev.Stage),
		CommText:       types.CommString(ev.Comm),
		Direction:      types.StageDirection(ev.Stage),
		TrafficScope:   deriveTrafficScope(src, dst),
		ObservedNode:   e.kr.LocalNode(),
		SrcIPText:      srcIP,
		DstIPText:      dstIP,
		ProtocolText:   types.IPProtocolName(ev.Protocol),
		Src:            src,
		Dst:            dst,
		DropReasonName: reasonName,
		DropCategory:   reasonCategory,
		DropStage:      reasonStage,
	}
}
