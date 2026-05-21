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

// memoryGen 은 polinux/stress 컨테이너를 target node 에 nodeName 으로 강제 배치해 stress --vm 1
// --vm-bytes <bytes> 로 heavy alloc 부하를 발생시킨다. memory limit 를 Intensity 와 동일하게 둬
// cgroup OOM 경계에 근접한 working_set 압박을 만든다. dominant_dimension 의 memory dimension 검증을
// 위한 합성 부하 모듈이다.
//
// 단위 해석은 K8s resource.Quantity 규약 (K8s 표준 SI / binary prefix) 을 단일 진실원으로 둔다.
// Intensity 문자열을 resource.ParseQuantity 로 한 번만 파싱해 bytes 정수로 변환한 뒤 cgroup
// memory limit 와 stress 의 --vm-bytes 양쪽에 동일한 bytes 값을 전달해 단위 해석 차이로 인한
// cgroup limit 와 실 alloc 의 불일치 (조기 OOM 또는 부하 약화) 를 차단한다.
type memoryGen struct {
	client  kubernetes.Interface
	mu      sync.Mutex
	spawned []types.NamespacedName
}

func (g *memoryGen) Kind() Kind { return KindMemory }

// Start 는 stress-memory-<target_pod> Pod 를 spawn 한다. Intensity 는 메모리 양 (예: "512M",
// "1G") 이며 stress 의 --vm-bytes 인자와 cgroup memory limit 양쪽에 적용된다. Duration 만큼 stress
// 가 자동 종료되며 injector main 의 Stop 호출 시 Pod 가 cleanup 된다.
func (g *memoryGen) Start(ctx context.Context, params Params) error {
	if params.TargetNode == "" {
		return fmt.Errorf("memory loadgen: target node is empty")
	}
	limitQuantity, err := resource.ParseQuantity(params.Intensity)
	if err != nil {
		return fmt.Errorf("memory loadgen: parse intensity %q: %w", params.Intensity, err)
	}
	bytes := limitQuantity.Value()
	if bytes <= 0 {
		return fmt.Errorf("memory loadgen: intensity must be positive bytes, got %s", params.Intensity)
	}

	name := sanitizeName("stress-memory", params.TargetPod)
	meta := commonPodMeta(name, params)
	meta.Labels["injector.kind"] = string(KindMemory)
	pod := &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			NodeName:      params.TargetNode,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "stress",
					Image: "polinux/stress:1.0.4",
					Command: []string{"stress",
						"--vm", "1",
						"--vm-bytes", fmt.Sprintf("%d", bytes),
						"--vm-keep",
						"--timeout", fmt.Sprintf("%ds", int(params.Duration.Seconds())),
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: limitQuantity,
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: limitQuantity,
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

func (g *memoryGen) Stop(ctx context.Context) error {
	g.mu.Lock()
	spawned := append([]types.NamespacedName(nil), g.spawned...)
	g.spawned = nil
	g.mu.Unlock()
	return deleteAll(ctx, g.client, spawned)
}

func (g *memoryGen) SpawnedPods() []types.NamespacedName {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]types.NamespacedName(nil), g.spawned...)
}
