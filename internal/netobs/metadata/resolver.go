package metadata

import (
	"context"
	"errors"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"netobs/internal/kube"
	"netobs/internal/netobs/drop"
	"netobs/internal/netobs/types"
)

type podCacheEntry struct {
	key string
	id  kube.PodIdentity
}

type serviceCacheEntry struct {
	key string
	id  kube.PodIdentity
}

type flowCacheEntry struct {
	Src kube.PodIdentity
	Dst kube.PodIdentity
}

type runtimeCacheEntry struct {
	ID       kube.PodIdentity
	LastSeen time.Time
}

type Resolver struct {
	localNode    string
	client       kubernetes.Interface
	startupErr   error
	resyncPeriod time.Duration

	synced atomic.Bool

	mu sync.RWMutex

	podByIP     map[string]podCacheEntry
	podIPsByKey map[string][]string

	serviceByIP     map[string]serviceCacheEntry
	serviceIPsByKey map[string][]string

	nodeByIP     map[string]string
	nodeIPsByKey map[string][]string

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

func NewResolver(localNode string, resyncPeriod time.Duration) *Resolver {
	r := &Resolver{
		localNode:       localNode,
		resyncPeriod:    resyncPeriod,
		podByIP:         make(map[string]podCacheEntry),
		podIPsByKey:     make(map[string][]string),
		serviceByIP:     make(map[string]serviceCacheEntry),
		serviceIPsByKey: make(map[string][]string),
		nodeByIP:        make(map[string]string),
		nodeIPsByKey:    make(map[string][]string),

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

	cfg, err := kubeConfig()
	if err != nil {
		r.startupErr = err
		return r
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		r.startupErr = err
		return r
	}

	r.client = clientset
	return r
}

func kubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}

	if kubeconfig := strings.TrimSpace(os.Getenv("KUBECONFIG")); kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	home, err := os.UserHomeDir()
	if err == nil {
		path := filepath.Join(home, ".kube", "config")
		if _, statErr := os.Stat(path); statErr == nil {
			return clientcmd.BuildConfigFromFlags("", path)
		}
	}

	return nil, errors.New("no in-cluster config and no kubeconfig found")
}

func (r *Resolver) Start(ctx context.Context) {
	if r.client == nil {
		log.Printf("metadata resolver disabled: %v", r.startupErr)
		return
	}

	factory := informers.NewSharedInformerFactory(r.client, r.resyncPeriod)

	podInformer := factory.Core().V1().Pods().Informer()
	serviceInformer := factory.Core().V1().Services().Informer()
	nodeInformer := factory.Core().V1().Nodes().Informer()

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			r.onUpsertPod(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			r.onUpsertPod(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			r.onDeletePod(obj)
		},
	})

	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			r.onUpsertService(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			r.onUpsertService(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			r.onDeleteService(obj)
		},
	})

	nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			r.onUpsertNode(obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			r.onUpsertNode(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			r.onDeleteNode(obj)
		},
	})

	factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(
		ctx.Done(),
		podInformer.HasSynced,
		serviceInformer.HasSynced,
		nodeInformer.HasSynced,
	) {
		log.Printf("metadata resolver initial sync failed")
		return
	}

	r.synced.Store(true)
	log.Printf("metadata resolver informer sync completed")

	<-ctx.Done()
}

// HasSynced는 Kubernetes informer 캐시가 초기 sync를 완료했는지 반환한다.
// client 초기화 실패로 resolver가 비활성 상태인 경우에도 false를 반환해
// /readyz가 해당 상태를 드러내도록 한다.
func (r *Resolver) HasSynced() bool {
	return r.synced.Load()
}

func (r *Resolver) onUpsertPod(obj interface{}) {
	pod, ok := extractPod(obj)
	if !ok || pod == nil {
		return
	}

	key := podKey(pod)
	ips := podIPs(*pod)
	id := podIdentity(*pod)

	r.mu.Lock()
	defer r.mu.Unlock()

	// 기존 IP 매핑 제거
	if oldIPs, exists := r.podIPsByKey[key]; exists {
		for _, ip := range oldIPs {
			if entry, ok := r.podByIP[ip]; ok && entry.key == key {
				delete(r.podByIP, ip)
			}
		}
	}

	// 현재 Pod에 IP가 없으면 캐시만 정리하고 종료
	if len(ips) == 0 {
		delete(r.podIPsByKey, key)
		return
	}

	for _, ip := range ips {
		r.podByIP[ip] = podCacheEntry{
			key: key,
			id:  id,
		}
	}
	r.podIPsByKey[key] = ips
}

func (r *Resolver) onDeletePod(obj interface{}) {
	pod, ok := extractPod(obj)
	if !ok || pod == nil {
		return
	}

	key := podKey(pod)

	r.mu.Lock()
	defer r.mu.Unlock()

	if oldIPs, exists := r.podIPsByKey[key]; exists {
		for _, ip := range oldIPs {
			if entry, ok := r.podByIP[ip]; ok && entry.key == key {
				delete(r.podByIP, ip)
			}
		}
		delete(r.podIPsByKey, key)
		return
	}

	// fallback: tombstone만 있고 key 캐시가 없는 경우
	for _, ip := range podIPs(*pod) {
		if entry, ok := r.podByIP[ip]; ok && entry.key == key {
			delete(r.podByIP, ip)
		}
	}
}

func (r *Resolver) onUpsertService(obj interface{}) {
	svc, ok := extractService(obj)
	if !ok || svc == nil {
		return
	}

	key := serviceKey(svc)
	ips := serviceIPs(*svc)
	id := serviceIdentity(*svc)

	r.mu.Lock()
	defer r.mu.Unlock()

	if oldIPs, exists := r.serviceIPsByKey[key]; exists {
		for _, ip := range oldIPs {
			if entry, ok := r.serviceByIP[ip]; ok && entry.key == key {
				delete(r.serviceByIP, ip)
			}
		}
	}

	if len(ips) == 0 {
		delete(r.serviceIPsByKey, key)
		return
	}

	for _, ip := range ips {
		r.serviceByIP[ip] = serviceCacheEntry{
			key: key,
			id:  id,
		}
	}
	r.serviceIPsByKey[key] = ips
}

func (r *Resolver) onDeleteService(obj interface{}) {
	svc, ok := extractService(obj)
	if !ok || svc == nil {
		return
	}

	key := serviceKey(svc)

	r.mu.Lock()
	defer r.mu.Unlock()

	if oldIPs, exists := r.serviceIPsByKey[key]; exists {
		for _, ip := range oldIPs {
			if entry, ok := r.serviceByIP[ip]; ok && entry.key == key {
				delete(r.serviceByIP, ip)
			}
		}
		delete(r.serviceIPsByKey, key)
		return
	}

	for _, ip := range serviceIPs(*svc) {
		if entry, ok := r.serviceByIP[ip]; ok && entry.key == key {
			delete(r.serviceByIP, ip)
		}
	}
}

func (r *Resolver) onUpsertNode(obj interface{}) {
	node, ok := extractNode(obj)
	if !ok || node == nil {
		return
	}

	key := nodeKey(node)
	ips := nodeIPs(*node)

	r.mu.Lock()
	defer r.mu.Unlock()

	if oldIPs, exists := r.nodeIPsByKey[key]; exists {
		for _, ip := range oldIPs {
			if name, ok := r.nodeByIP[ip]; ok && name == node.Name {
				delete(r.nodeByIP, ip)
			}
		}
	}

	if len(ips) == 0 {
		delete(r.nodeIPsByKey, key)
		return
	}

	for _, ip := range ips {
		r.nodeByIP[ip] = node.Name
	}
	r.nodeIPsByKey[key] = ips
}

func (r *Resolver) onDeleteNode(obj interface{}) {
	node, ok := extractNode(obj)
	if !ok || node == nil {
		return
	}

	key := nodeKey(node)

	r.mu.Lock()
	defer r.mu.Unlock()

	if oldIPs, exists := r.nodeIPsByKey[key]; exists {
		for _, ip := range oldIPs {
			if name, ok := r.nodeByIP[ip]; ok && name == node.Name {
				delete(r.nodeByIP, ip)
			}
		}
		delete(r.nodeIPsByKey, key)
		return
	}

	for _, ip := range nodeIPs(*node) {
		if name, ok := r.nodeByIP[ip]; ok && name == node.Name {
			delete(r.nodeByIP, ip)
		}
	}
}

func extractPod(obj interface{}) (*corev1.Pod, bool) {
	switch t := obj.(type) {
	case *corev1.Pod:
		return t, true
	case cache.DeletedFinalStateUnknown:
		pod, ok := t.Obj.(*corev1.Pod)
		return pod, ok
	default:
		return nil, false
	}
}

func extractService(obj interface{}) (*corev1.Service, bool) {
	switch t := obj.(type) {
	case *corev1.Service:
		return t, true
	case cache.DeletedFinalStateUnknown:
		svc, ok := t.Obj.(*corev1.Service)
		return svc, ok
	default:
		return nil, false
	}
}

func extractNode(obj interface{}) (*corev1.Node, bool) {
	switch t := obj.(type) {
	case *corev1.Node:
		return t, true
	case cache.DeletedFinalStateUnknown:
		node, ok := t.Obj.(*corev1.Node)
		return node, ok
	default:
		return nil, false
	}
}

func podKey(p *corev1.Pod) string {
	if p.UID != "" {
		return string(p.UID)
	}
	return p.Namespace + "/" + p.Name
}

func serviceKey(s *corev1.Service) string {
	if s.UID != "" {
		return string(s.UID)
	}
	return s.Namespace + "/" + s.Name
}

func nodeKey(n *corev1.Node) string {
	if n.UID != "" {
		return string(n.UID)
	}
	return n.Name
}

func podIPs(p corev1.Pod) []string {
	seen := make(map[string]struct{}, 1+len(p.Status.PodIPs))
	out := make([]string, 0, 1+len(p.Status.PodIPs))

	if p.Status.PodIP != "" {
		seen[p.Status.PodIP] = struct{}{}
		out = append(out, p.Status.PodIP)
	}

	for _, pip := range p.Status.PodIPs {
		if pip.IP == "" {
			continue
		}
		if _, exists := seen[pip.IP]; exists {
			continue
		}
		seen[pip.IP] = struct{}{}
		out = append(out, pip.IP)
	}

	return out
}

func serviceIPs(s corev1.Service) []string {
	seen := make(map[string]struct{}, 1+len(s.Spec.ClusterIPs))
	out := make([]string, 0, 1+len(s.Spec.ClusterIPs))

	if s.Spec.ClusterIP != "" && s.Spec.ClusterIP != "None" {
		seen[s.Spec.ClusterIP] = struct{}{}
		out = append(out, s.Spec.ClusterIP)
	}

	for _, ip := range s.Spec.ClusterIPs {
		if ip == "" || ip == "None" {
			continue
		}
		if _, exists := seen[ip]; exists {
			continue
		}
		seen[ip] = struct{}{}
		out = append(out, ip)
	}

	return out
}

func nodeIPs(n corev1.Node) []string {
	seen := make(map[string]struct{}, len(n.Status.Addresses))
	out := make([]string, 0, len(n.Status.Addresses))

	for _, addr := range n.Status.Addresses {
		if addr.Address == "" {
			continue
		}
		if _, exists := seen[addr.Address]; exists {
			continue
		}
		seen[addr.Address] = struct{}{}
		out = append(out, addr.Address)
	}

	return out
}

func podIdentity(p corev1.Pod) kube.PodIdentity {
	kind, workload := ownerInfo(p)

	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     p.Namespace,
		PodUID:        string(p.UID),
		PodName:       p.Name,
		NodeName:      p.Spec.NodeName,
		WorkloadKind:  kind,
		Workload:      workload,
		PodIP:         p.Status.PodIP,
	}
}

func serviceIdentity(s corev1.Service) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassService,
		Namespace:     s.Namespace,
		WorkloadKind:  "Service",
		Workload:      s.Name,
		PodIP:         s.Spec.ClusterIP,
	}
}

func nodeIdentity(nodeName, ip string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassNode,
		NodeName:      nodeName,
		WorkloadKind:  "Node",
		Workload:      nodeName,
		PodIP:         ip,
	}
}

func externalIdentity(ip string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassExternal,
		WorkloadKind:  "External",
		Workload:      "external",
		PodIP:         ip,
	}
}

func unresolvedIdentity(ip string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassUnresolved,
		WorkloadKind:  "Unresolved",
		Workload:      "unresolved",
		PodIP:         ip,
	}
}

func ownerInfo(p corev1.Pod) (string, string) {
	if len(p.OwnerReferences) == 0 {
		return "Pod", p.Name
	}

	owner := p.OwnerReferences[0]
	kind := owner.Kind
	name := owner.Name

	if kind == "ReplicaSet" {
		if dep := kube.TrimGeneratedSuffix(name); dep != "" {
			return "Deployment", dep
		}
	}

	return kind, name
}

// identityCompleteness는 식별 필드가 얼마나 채워졌는지 점수화한다.
// 같은 IdentityClass 내에서 tiebreak 용도로만 쓰인다.
func identityCompleteness(p kube.PodIdentity) int {
	score := 0
	if p.Namespace != "" {
		score++
	}
	if p.PodUID != "" {
		score++
	}
	if p.PodName != "" {
		score++
	}
	if p.NodeName != "" {
		score++
	}
	if p.Workload != "" {
		score++
	}
	if p.WorkloadKind != "" {
		score++
	}
	if p.PodIP != "" {
		score++
	}
	return score
}

func strongerIdentity(current, candidate kube.PodIdentity) kube.PodIdentity {
	if candidate.Rank() > current.Rank() {
		return candidate
	}
	if current.Rank() > candidate.Rank() {
		return current
	}

	if identityCompleteness(candidate) > identityCompleteness(current) {
		return candidate
	}
	return current
}

func withObservedIP(id kube.PodIdentity, ip string) kube.PodIdentity {
	if ip != "" {
		id.PodIP = ip
	}
	return id
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

func (r *Resolver) resolveIP(ip string) kube.PodIdentity {
	if ip == "" {
		return unresolvedIdentity(ip)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if entry, ok := r.podByIP[ip]; ok {
		return entry.id
	}
	if entry, ok := r.serviceByIP[ip]; ok {
		return entry.id
	}
	if nodeName, ok := r.nodeByIP[ip]; ok {
		return nodeIdentity(nodeName, ip)
	}
	return classifyFallbackIP(ip)
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
			src = strongerIdentity(src, withObservedIP(id, srcIP))
		}
	}
	if !src.IsPod() && ev.Ifindex != 0 {
		if id, ok := r.lookupIfindexHint(ev.Ifindex, now); ok {
			src = strongerIdentity(src, withObservedIP(id, srcIP))
		}
	}
	if !dst.IsPod() && ev.SkbIif != 0 {
		if id, ok := r.lookupIfindexHint(ev.SkbIif, now); ok {
			dst = strongerIdentity(dst, withObservedIP(id, dstIP))
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

func classifyFallbackIP(ip string) kube.PodIdentity {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return unresolvedIdentity(ip)
	}

	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return unresolvedIdentity(ip)
	}

	// RFC1918 / ULA 등 private 주소인데 pod/service/node 어느 쪽에도 매핑되지 않음
	// -> cluster 내부 또는 host 내부일 가능성이 높으므로 unresolved로 정리
	if addr.IsPrivate() {
		return unresolvedIdentity(ip)
	}

	// public IP면 external로 분류
	return externalIdentity(ip)
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

func (r *Resolver) Enrich(ev types.Event, mapper *drop.Mapper) types.EnrichedEvent {
	srcIP := types.U32ToIPv4(ev.Saddr)
	dstIP := types.U32ToIPv4(ev.Daddr)

	now := time.Now()

	src := r.resolveIP(srcIP)
	dst := r.resolveIP(dstIP)

	if cached, ok := r.lookupFlow(ev.SocketCookie); ok {
		src = strongerIdentity(src, withObservedIP(cached.Src, srcIP))
		dst = strongerIdentity(dst, withObservedIP(cached.Dst, dstIP))
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
		ObservedNode:   r.localNode,
		SrcIPText:      srcIP,
		DstIPText:      dstIP,
		Src:            src,
		Dst:            dst,
		DropReasonName: reasonName,
		DropCategory:   reasonCategory,
	}
}
