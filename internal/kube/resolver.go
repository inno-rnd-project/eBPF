package kube

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
)

// Resolver는 클러스터의 Pod/Service/Node IP 인덱스를 informer로 유지하고
// IP → PodIdentity 해석을 제공한다. netobs(Event 기반 enrich)와 gpuobs(PID 기반 Pod 귀속)
// 양쪽 모두가 동일한 인덱스를 공유할 수 있도록 공용 패키지에 둔다.
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
}

type podCacheEntry struct {
	key string
	id  PodIdentity
}

type serviceCacheEntry struct {
	key string
	id  PodIdentity
}

// NewResolver는 in-cluster 또는 kubeconfig에서 client를 구성한 Resolver를 반환한다.
// client 초기화에 실패해도 Resolver 자체는 비활성 상태로 반환되며, Start 시 disabled 로그를 남긴다.
// 이 graceful 동작은 클러스터 외부 개발 환경에서 바이너리가 멈추지 않게 한다.
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

// LocalNode는 NewResolver에 전달된 관측 노드 이름을 반환한다.
// netobs Enricher가 ObservedNode 필드 기록에 사용한다.
func (r *Resolver) LocalNode() string {
	return r.localNode
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

// Start는 Pod/Service/Node informer를 띄우고 초기 sync가 완료될 때까지 대기한 뒤 ctx 종료까지 블록된다.
// client 미구성 시 disabled 로그만 남기고 즉시 반환해 호출자가 별도 분기 없이 goroutine으로 실행해도 된다.
func (r *Resolver) Start(ctx context.Context) {
	if r.client == nil {
		log.Printf("kube resolver disabled: %v", r.startupErr)
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
		log.Printf("kube resolver initial sync failed")
		return
	}

	r.synced.Store(true)
	log.Printf("kube resolver informer sync completed")

	<-ctx.Done()
}

// HasSynced는 informer 캐시가 초기 sync를 완료했는지 반환한다.
// client 미구성으로 비활성 상태일 때도 false를 반환해 /readyz가 그 상태를 드러낼 수 있게 한다.
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

	if oldIPs, exists := r.podIPsByKey[key]; exists {
		for _, ip := range oldIPs {
			if entry, ok := r.podByIP[ip]; ok && entry.key == key {
				delete(r.podByIP, ip)
			}
		}
	}

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

func podIdentity(p corev1.Pod) PodIdentity {
	kind, workload := ownerInfo(p)

	return PodIdentity{
		IdentityClass: IdentityClassPod,
		Namespace:     p.Namespace,
		PodUID:        string(p.UID),
		PodName:       p.Name,
		NodeName:      p.Spec.NodeName,
		WorkloadKind:  kind,
		Workload:      workload,
		PodIP:         p.Status.PodIP,
	}
}

func serviceIdentity(s corev1.Service) PodIdentity {
	return PodIdentity{
		IdentityClass: IdentityClassService,
		Namespace:     s.Namespace,
		WorkloadKind:  "Service",
		Workload:      s.Name,
		PodIP:         s.Spec.ClusterIP,
	}
}

func nodeIdentity(nodeName, ip string) PodIdentity {
	return PodIdentity{
		IdentityClass: IdentityClassNode,
		NodeName:      nodeName,
		WorkloadKind:  "Node",
		Workload:      nodeName,
		PodIP:         ip,
	}
}

func externalIdentity(ip string) PodIdentity {
	return PodIdentity{
		IdentityClass: IdentityClassExternal,
		WorkloadKind:  "External",
		Workload:      "external",
		PodIP:         ip,
	}
}

func unresolvedIdentity(ip string) PodIdentity {
	return PodIdentity{
		IdentityClass: IdentityClassUnresolved,
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
		if dep := TrimGeneratedSuffix(name); dep != "" {
			return "Deployment", dep
		}
	}

	return kind, name
}

// identityCompleteness는 식별 필드가 얼마나 채워졌는지 점수화한다.
// 같은 IdentityClass 내에서 StrongerIdentity의 tiebreak 용도로만 쓰인다.
func identityCompleteness(p PodIdentity) int {
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

// StrongerIdentity는 두 PodIdentity 중 더 신뢰도가 높은 쪽을 반환한다.
// Rank가 우선이며, 동률일 때는 채워진 필드 수가 많은 쪽을 택한다.
// netobs Enricher가 IP 해석 결과와 flow/runtime hint를 병합할 때 사용한다.
func StrongerIdentity(current, candidate PodIdentity) PodIdentity {
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

// WithObservedIP는 PodIdentity의 PodIP를 관측된 값으로 보강해 반환한다.
// hint 캐시에 기록된 식별이 PodIP를 비워둘 수 있어, 호출 시점의 실제 IP로 채워준다.
func WithObservedIP(id PodIdentity, ip string) PodIdentity {
	if ip != "" {
		id.PodIP = ip
	}
	return id
}

// ResolveIP는 IP 문자열을 PodIdentity로 해석한다.
// 해석 우선순위는 Pod → Service → Node → 사설/특수 주소(unresolved) → public(external) 순이다.
func (r *Resolver) ResolveIP(ip string) PodIdentity {
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

// classifyFallbackIP는 인덱스에서 매칭이 없는 IP를 분류한다.
// loopback/multicast/link-local/unspecified 및 RFC1918 등 사설 주소는 unresolved,
// 그 외 public IP는 external로 분류한다.
func classifyFallbackIP(ip string) PodIdentity {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return unresolvedIdentity(ip)
	}

	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return unresolvedIdentity(ip)
	}

	if addr.IsPrivate() {
		return unresolvedIdentity(ip)
	}

	return externalIdentity(ip)
}
