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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"netobs/internal/drop"
	"netobs/internal/types"
)

type Resolver struct {
	localNode  string
	refresh    time.Duration
	client     kubernetes.Interface
	startupErr error

	mu      sync.RWMutex
	podByIP map[string]types.PodIdentity
}

func NewResolver(localNode string, refresh time.Duration) *Resolver {
	r := &Resolver{
		localNode: localNode,
		refresh:   refresh,
		podByIP:   make(map[string]types.PodIdentity),
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

	if err := r.refreshOnce(ctx); err != nil {
		log.Printf("metadata resolver initial refresh failed: %v", err)
	} else {
		log.Printf("metadata resolver initial refresh completed")
	}

	ticker := time.NewTicker(r.refresh)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.refreshOnce(ctx); err != nil {
				log.Printf("metadata resolver refresh failed: %v", err)
			}
		}
	}
}

func (r *Resolver) refreshOnce(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pods, err := r.client.CoreV1().Pods("").List(cctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	next := make(map[string]types.PodIdentity, len(pods.Items))

	for _, p := range pods.Items {
		if p.Status.PodIP == "" && len(p.Status.PodIPs) == 0 {
			continue
		}

		id := podIdentity(p)

		if p.Status.PodIP != "" {
			next[p.Status.PodIP] = id
		}
		for _, pip := range p.Status.PodIPs {
			if pip.IP != "" {
				next[pip.IP] = id
			}
		}
	}

	r.mu.Lock()
	r.podByIP = next
	r.mu.Unlock()
	return nil
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

	if id, ok := r.podByIP[ip]; ok {
		return id
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
