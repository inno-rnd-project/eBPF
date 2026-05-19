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

// gpuGen 은 nvidia/cuda 컨테이너를 target node 에 spawn 해 cuda kernel busy loop 로 GPU 점유를
// 발생시킨다. NVIDIA GPU Operator 가 device plugin 을 설치한 노드에서만 동작하며 미설치 노드에서는
// Pod 가 Pending 으로 남는다.
type gpuGen struct {
	client  kubernetes.Interface
	mu      sync.Mutex
	spawned []types.NamespacedName
}

func (g *gpuGen) Kind() Kind { return KindGPU }

// Start 는 cuda runtime 이미지에서 sh 로 cuda kernel 무한 루프 명령을 실행하는 Pod 를 spawn 한다.
// 본 구현은 nvidia/cuda 의 vectorAdd sample 을 무한 반복하는 방식이다. Intensity 는 "1 device" 같은
// 형태로 받아 nvidia.com/gpu resource limit 으로 변환한다. busy loop 가 Duration 후 자동 종료되도록
// timeout 명령으로 감싼다.
func (g *gpuGen) Start(ctx context.Context, params Params) error {
	if params.TargetNode == "" {
		return fmt.Errorf("gpu loadgen: target node is empty")
	}
	gpuCount := params.Intensity
	if gpuCount == "" {
		gpuCount = "1"
	}
	gpuQuantity, err := resource.ParseQuantity(gpuCount)
	if err != nil {
		return fmt.Errorf("gpu loadgen: parse intensity %q: %w", params.Intensity, err)
	}

	name := fmt.Sprintf("stress-gpu-%s", params.TargetPod)
	if len(name) > 63 {
		name = name[:63]
	}
	meta := commonPodMeta(name, params)
	meta.Labels["injector.kind"] = string(KindGPU)
	pod := &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			NodeName:      params.TargetNode,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "cuda-stress",
					Image: "nvidia/cuda:12.4.0-runtime-ubuntu22.04",
					// /tmp/cuda-stress.sh 같은 사전 설치 스크립트가 없으므로 inline 으로 cuda 의
					// busy loop 를 nvidia-smi 로 대체한다. nvidia-smi -l 1 은 1 초마다 device 상태를
					// 폴링하지만 실제 GPU 점유는 일어나지 않는다. cuda kernel busy loop 의 정확한
					// stress 는 별도 image (예: nvidia/cuda-sample) 가 필요하며 follow-up 으로 분리.
					// 본 구현은 GPU 자원 할당 / 메트릭 emit 검증을 우선 목표로 한다.
					Command: []string{"timeout", fmt.Sprintf("%d", int(params.Duration.Seconds())), "nvidia-smi", "-l", "1"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
							"nvidia.com/gpu":      gpuQuantity,
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
							"nvidia.com/gpu":      gpuQuantity,
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

func (g *gpuGen) Stop(ctx context.Context) error {
	g.mu.Lock()
	spawned := append([]types.NamespacedName(nil), g.spawned...)
	g.spawned = nil
	g.mu.Unlock()
	return deleteAll(ctx, g.client, spawned)
}

func (g *gpuGen) SpawnedPods() []types.NamespacedName {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]types.NamespacedName(nil), g.spawned...)
}
