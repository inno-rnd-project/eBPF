package loadgen

import (
	"context"
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// cpuGen 은 polinux/stress 컨테이너를 target node 에 nodeName 으로 강제 배치해 stress --cpu N 으로
// CPU 부하를 발생시킨다. cpu limit 을 Intensity 로 두어 cgroup throttling 을 강제 발생시킨다.
type cpuGen struct {
	client  kubernetes.Interface
	mu      sync.Mutex
	spawned []types.NamespacedName
}

func (g *cpuGen) Kind() Kind { return KindCPU }

// Start 는 stress-cpu-<target_pod> Pod 를 spawn 한다. stress --cpu N 의 N 은 limit / 250m 으로 산출
// 해 worker thread 가 limit 의 4 배가 되도록 둔다 (cgroup throttling 강제). Duration 만큼 stress 가
// 자동 종료되며 injector main 의 Stop 호출 시 Pod 가 cleanup 된다.
func (g *cpuGen) Start(ctx context.Context, params Params) error {
	if params.TargetNode == "" {
		return fmt.Errorf("cpu loadgen: target node is empty")
	}
	limitQuantity, err := resource.ParseQuantity(params.Intensity)
	if err != nil {
		return fmt.Errorf("cpu loadgen: parse intensity %q: %w", params.Intensity, err)
	}
	limitMilli := limitQuantity.MilliValue()
	if limitMilli <= 0 {
		return fmt.Errorf("cpu loadgen: intensity must be positive milliCPU, got %s", params.Intensity)
	}
	workers := limitMilli / 250
	if workers < 1 {
		workers = 1
	}

	name := sanitizeName("stress-cpu", params.TargetPod)
	meta := commonPodMeta(name, params)
	meta.Labels["injector.kind"] = string(KindCPU)
	pod := &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			NodeName:      params.TargetNode,
			RestartPolicy: corev1.RestartPolicyNever,
			// #418 컨트롤러 부재 시에도 kubelet 이 강제 종료하는 고아 방지 상한.
			ActiveDeadlineSeconds: activeDeadlineSeconds(params),
			Containers: []corev1.Container{
				{
					Name:  "stress",
					Image: "polinux/stress:1.0.4",
					Command: []string{"stress",
						"--cpu", fmt.Sprintf("%d", workers),
						"--timeout", fmt.Sprintf("%ds", int(params.Duration.Seconds())),
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    limitQuantity,
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	if err := createPod(ctx, g.client, pod); err != nil {
		return err
	}
	g.mu.Lock()
	g.spawned = append(g.spawned, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
	g.mu.Unlock()
	return nil
}

func (g *cpuGen) Stop(ctx context.Context) error {
	g.mu.Lock()
	spawned := append([]types.NamespacedName(nil), g.spawned...)
	g.spawned = nil
	g.mu.Unlock()
	return deleteAll(ctx, g.client, spawned)
}

func (g *cpuGen) SpawnedPods() []types.NamespacedName {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]types.NamespacedName(nil), g.spawned...)
}
