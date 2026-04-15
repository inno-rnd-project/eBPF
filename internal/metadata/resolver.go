package metadata

import (
	"context"
	"errors"
	"log"
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

type Resolver struct {
	localNode  string
	client     kubernetes.Interface
	startupErr error

	mu          sync.RWMutex
	podByIP     map[string]podCacheEntry
	podIPsByKey map[string][]string
}

func NewResolver(localNode string, _ time.Duration) *Resolver {
	r := &Resolver{
		localNode:   localNode,
		podByIP:     make(map[string]podCacheEntry),
		podIPsByKey: make(map[string][]string),
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

	factory.Start(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced) {
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

func podKey(p *corev1.Pod) string {
	if p.UID != "" {
		return string(p.UID)
	}
	return p.Namespace + "/" + p.Name
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

func podIdentity(p corev1.Pod) types.PodIdentity {
	kind, workload := ownerInfo(p)

	return types.PodIdentity{
		Namespace:    p.Namespace,
		PodName:      p.Name,
		NodeName:     p.Spec.NodeName,
		WorkloadKind: kind,
		Workload:     workload,
		PodIP:        p.Status.PodIP,
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
	r.mu.RLock()
	defer r.mu.RUnlock()

	if entry, ok := r.podByIP[ip]; ok {
		return entry.id
	}

	return types.PodIdentity{PodIP: ip}
}

func deriveTrafficScope(src, dst types.PodIdentity) string {
	switch {
	case src.Known() && dst.Known():
		if src.NodeName != "" && dst.NodeName != "" && src.NodeName == dst.NodeName {
			return "same_node"
		}
		if src.NodeName != "" && dst.NodeName != "" && src.NodeName != dst.NodeName {
			return "cross_node"
		}
		return "pod_to_pod"
	case src.Known() && !dst.Known():
		return "to_external"
	case !src.Known() && dst.Known():
		return "from_external"
	default:
		return "unknown"
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
