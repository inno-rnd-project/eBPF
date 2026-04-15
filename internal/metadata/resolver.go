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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"netobs/internal/drop"
	"netobs/internal/types"
)

type podCacheEntry struct {
	key string
	id  types.PodIdentity
}

type serviceCacheEntry struct {
	key string
	id  types.PodIdentity
}

type Resolver struct {
	localNode  string
	client     kubernetes.Interface
	startupErr error

	mu sync.RWMutex

	podByIP     map[string]podCacheEntry
	podIPsByKey map[string][]string

	serviceByIP     map[string]serviceCacheEntry
	serviceIPsByKey map[string][]string

	nodeByIP     map[string]string
	nodeIPsByKey map[string][]string
}

func NewResolver(localNode string, _ time.Duration) *Resolver {
	r := &Resolver{
		localNode:       localNode,
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

	factory := informers.NewSharedInformerFactory(r.client, 0)

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

	log.Printf("metadata resolver informer sync completed")

	<-ctx.Done()
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

func podIdentity(p corev1.Pod) types.PodIdentity {
	kind, workload := ownerInfo(p)

	return types.PodIdentity{
		IdentityClass: types.IdentityClassPod,
		Namespace:     p.Namespace,
		PodName:       p.Name,
		NodeName:      p.Spec.NodeName,
		WorkloadKind:  kind,
		Workload:      workload,
		PodIP:         p.Status.PodIP,
	}
}

func serviceIdentity(s corev1.Service) types.PodIdentity {
	return types.PodIdentity{
		IdentityClass: types.IdentityClassService,
		Namespace:     s.Namespace,
		WorkloadKind:  "Service",
		Workload:      s.Name,
		PodIP:         s.Spec.ClusterIP,
	}
}

func nodeIdentity(nodeName, ip string) types.PodIdentity {
	return types.PodIdentity{
		IdentityClass: types.IdentityClassNode,
		NodeName:      nodeName,
		WorkloadKind:  "Node",
		Workload:      nodeName,
		PodIP:         ip,
	}
}

func externalIdentity(ip string) types.PodIdentity {
	return types.PodIdentity{
		IdentityClass: types.IdentityClassExternal,
		WorkloadKind:  "External",
		Workload:      "external",
		PodIP:         ip,
	}
}

func unresolvedIdentity(ip string) types.PodIdentity {
	return types.PodIdentity{
		IdentityClass: types.IdentityClassUnresolved,
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
		if dep := trimReplicaSetHash(name); dep != "" {
			return "Deployment", dep
		}
	}

	return kind, name
}

func trimReplicaSetHash(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return ""
	}

	last := parts[len(parts)-1]
	if len(last) < 8 {
		return ""
	}

	for _, ch := range last {
		if !((ch >= 'a' && ch <= 'f') || (ch >= '0' && ch <= '9')) {
			return ""
		}
	}

	return strings.Join(parts[:len(parts)-1], "-")
}

func (r *Resolver) resolveIP(ip string) types.PodIdentity {
	if ip == "" {
		return unresolvedIdentity(ip)
	}

	r.mu.RLock()
	if entry, ok := r.podByIP[ip]; ok {
		r.mu.RUnlock()
		return entry.id
	}
	if entry, ok := r.serviceByIP[ip]; ok {
		r.mu.RUnlock()
		return entry.id
	}
	if nodeName, ok := r.nodeByIP[ip]; ok {
		r.mu.RUnlock()
		return nodeIdentity(nodeName, ip)
	}
	r.mu.RUnlock()

	return classifyFallbackIP(ip)
}

func classifyFallbackIP(ip string) types.PodIdentity {
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

func deriveTrafficScope(src, dst types.PodIdentity) string {
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

	src := r.resolveIP(srcIP)
	dst := r.resolveIP(dstIP)

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
