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

// networkGen 은 iperf3 server 를 target node 에 두고 client 를 다른 노드에 두어 server 로 트래픽을
// 발사한다. server Pod 의 hostIP 를 사용하면 Service 없이도 client 가 직접 접근 가능하지만 본 모듈은
// 단순화를 위해 server Pod 의 Pod IP 를 사용하지 않고 hostNetwork 도 쓰지 않는다. 대신 server 가
// 먼저 spawn 되어 ready 되면 client 가 server 의 Pod IP 로 접근한다. 본 단순 구조는 단위 테스트에서
// fake clientset 으로 두 Pod 의 create 만 검증한다.
type networkGen struct {
	client  kubernetes.Interface
	mu      sync.Mutex
	spawned []types.NamespacedName
}

func (g *networkGen) Kind() Kind { return KindNetwork }

// Start 는 iperf3 server Pod 를 target node 에 spawn 한 뒤 client Pod 를 별도 노드에 spawn 한다.
// client 의 iperf3 명령은 server Pod 의 DNS 이름 (네임스페이스 내 Pod fqdn) 으로 접근하도록 둔다.
// Duration 의 초 단위만큼 iperf3 -t 옵션으로 자동 종료 후 Pod 가 Succeeded 상태가 된다.
func (g *networkGen) Start(ctx context.Context, params Params) error {
	if params.TargetNode == "" {
		return fmt.Errorf("network loadgen: target node is empty")
	}
	bandwidth := params.Intensity
	if bandwidth == "" {
		bandwidth = "100M"
	}

	serverName := sanitizeName("stress-net-server", params.TargetPod)
	clientName := sanitizeName("stress-net-client", params.TargetPod)

	serverMeta := commonPodMeta(serverName, params)
	serverMeta.Labels["injector.kind"] = string(KindNetwork)
	serverMeta.Labels["injector.role"] = "server"
	server := &corev1.Pod{
		ObjectMeta: serverMeta,
		Spec: corev1.PodSpec{
			NodeName:      params.TargetNode,
			RestartPolicy: corev1.RestartPolicyNever,
			Subdomain:     "workload-injector",
			Hostname:      serverName,
			Containers: []corev1.Container{
				{
					Name:  "iperf3",
					Image: "networkstatic/iperf3:3.16",
					// -1 (one-off) 옵션은 첫 connection 종료 후 server process 가 즉시 종료된다.
					// client 의 nc -z probe 가 첫 connection 으로 인식되어 server 가 종료된 뒤
					// 실제 iperf3 -c 가 실패하므로 제거하고 injector binary 의 Stop 호출이 Pod
					// delete 로 정상 cleanup 한다.
					Command: []string{"iperf3", "-s"},
					Ports: []corev1.ContainerPort{
						{ContainerPort: 5201, Name: "iperf3", Protocol: corev1.ProtocolTCP},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	if err := createPod(ctx, g.client, server); err != nil {
		return err
	}
	g.mu.Lock()
	g.spawned = append(g.spawned, types.NamespacedName{Namespace: server.Namespace, Name: server.Name})
	g.mu.Unlock()

	clientMeta := commonPodMeta(clientName, params)
	clientMeta.Labels["injector.kind"] = string(KindNetwork)
	clientMeta.Labels["injector.role"] = "client"
	// server Pod 의 DNS 이름. Pod 가 spawn 된 후 Endpoints 가 만들어지는 시점은 K8s scheduler 에
	// 달려 있어 client 의 iperf3 가 즉시 connect 실패할 수 있다. iperf3 client 는 자동 재시도가
	// 없으므로 sh -c 의 until 루프로 30 초 안에 ready 되는지 polling 후 트래픽 발사한다.
	// 상대 DNS 이름 (FQDN 대신) 으로 둬 cluster 의 DNS suffix 가 cluster.local 외의 다른 도메인
	// 이라도 ndots:5 search 룰로 자동 해석되도록 한다. client 와 server 가 같은 namespace
	// (params.SpawnNamespace) 라 추가 namespace 한정도 불필요하다.
	serverDNS := fmt.Sprintf("%s.workload-injector", serverName)
	client := &corev1.Pod{
		ObjectMeta: clientMeta,
		Spec: corev1.PodSpec{
			// 자동 cleanup 위해 nodeName 비워두면 임의 노드 스케줄. target node 와 다른 노드에
			// 배치되도록 nodeAffinity 로 강제하는 패턴은 follow-up 으로 분리. 본 구현은 단순화.
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "iperf3",
					Image: "networkstatic/iperf3:3.16",
					Command: []string{"sh", "-c", fmt.Sprintf(
						"until nc -z %s 5201 2>/dev/null; do sleep 1; done; iperf3 -c %s -t %d -b %s",
						serverDNS, serverDNS, int(params.Duration.Seconds()), bandwidth,
					)},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
	}
	if err := createPod(ctx, g.client, client); err != nil {
		// server 만 spawn 되고 client 가 실패한 경우 server cleanup.
		_ = deletePod(ctx, g.client, types.NamespacedName{Namespace: server.Namespace, Name: server.Name})
		g.mu.Lock()
		g.spawned = nil
		g.mu.Unlock()
		return err
	}
	g.mu.Lock()
	g.spawned = append(g.spawned, types.NamespacedName{Namespace: client.Namespace, Name: client.Name})
	g.mu.Unlock()
	return nil
}

func (g *networkGen) Stop(ctx context.Context) error {
	g.mu.Lock()
	spawned := append([]types.NamespacedName(nil), g.spawned...)
	g.spawned = nil
	g.mu.Unlock()
	return deleteAll(ctx, g.client, spawned)
}

func (g *networkGen) SpawnedPods() []types.NamespacedName {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]types.NamespacedName(nil), g.spawned...)
}
