package loadgen

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// bandwidthPattern 은 sh -c 스크립트에 결합을 허용하는 iperf3 -b 값의 형태다 (#253). 셸 안전성이
// safety.CheckIntensity (checkBandwidth) 에만 의존하면 그쪽이 다른 -b 형식 지원 등으로 완화될 때
// 임의 명령 실행 경로가 조용히 열리므로, 문자열 결합의 당사자인 본 패키지 경계에서 재검증한다.
var bandwidthPattern = regexp.MustCompile(`^[0-9]+[KkMmGg]?$`)

// serverTimeoutGraceSec 는 iperf3 server 의 timeout wrapper 여유분이다 (#418). client 의 ready
// polling 과 트래픽 꼬리를 덮은 뒤 스스로 종료한다. activeDeadlineSeconds (동일 여유) 보다 먼저
// 프로세스 단에서 끝나는 1차 장치이며 deadline 은 이미지 pull 지연 등까지 덮는 2차 장치다.
const serverTimeoutGraceSec = 90

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
	// safety.parseBandwidthBps 가 TrimSpace 후 검증하므로 " 100M " 같은 공백 포함 값이 safety 를
	// 통과한다. loadgen 도 동일하게 trim 후 재검증해 safety 를 통과한 정상 입력이 여기서 거부되는
	// 불일치를 막는다.
	bandwidth := strings.TrimSpace(params.Intensity)
	if bandwidth == "" {
		bandwidth = "100M"
	}
	// sh -c 결합 전 재검증 (#253). client 조립 직전이 아니라 여기서 fail-fast 해 server Pod 만
	// spawn 된 채 실패하는 고아 경로도 함께 막는다.
	if !bandwidthPattern.MatchString(bandwidth) {
		return fmt.Errorf("network loadgen: invalid bandwidth %q (want digits with optional K/M/G suffix)", bandwidth)
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
			// #418 컨트롤러 부재 시에도 kubelet 이 강제 종료하는 고아 방지 상한.
			ActiveDeadlineSeconds: activeDeadlineSeconds(params),
			Subdomain:             "workload-injector",
			Hostname:              serverName,
			Containers: []corev1.Container{
				{
					Name:  "iperf3",
					Image: "mlabbe/iperf3:3.16-r0",
					// #418 timeout wrapper 로 duration + 여유 뒤 스스로 종료한다. -1 (one-off)
					// 은 첫 connection 종료 후 server process 가 즉시 죽어 client 의 nc -z probe
					// 가 그 첫 connection 을 소비하는 문제로 제거됐는데 (#253 이전), timeout 방식
					// 은 multi-connection 을 유지하므로 probe 는 종전대로 안전하다. 정상 경로는
					// injector 의 Stop delete 가 먼저 오고 본 wrapper 는 컨트롤러 부재 시의 자체
					// 종료 장치다. grace (90s) 는 client 의 ready polling 을 덮으면서
					// activeDeadlineSeconds (120s 여유) 보다 먼저 프로세스 단에서 끝나게 둔다.
					Command: []string{"sh", "-c", fmt.Sprintf(
						"exec timeout %d iperf3 -s", int(params.Duration.Seconds())+serverTimeoutGraceSec,
					)},
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
			RestartPolicy: corev1.RestartPolicyNever,
			// #418 컨트롤러 부재 시에도 kubelet 이 강제 종료하는 고아 방지 상한.
			ActiveDeadlineSeconds: activeDeadlineSeconds(params),
			// client 가 우연히 target node 에 스케줄되면 두 Pod 사이 트래픽이 노드 NIC 를 거치지
			// 않아 network 부하의 의미 (cross-node bandwidth 경쟁) 가 사라진다. nodeAffinity 의
			// NotIn 으로 target node 를 명시적 회피한다. multi-node cluster 가 전제이며 다른 노드가
			// 없으면 client Pod 가 Pending 으로 남는다.
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{
							{
								MatchExpressions: []corev1.NodeSelectorRequirement{
									{
										Key:      "kubernetes.io/hostname",
										Operator: corev1.NodeSelectorOpNotIn,
										Values:   []string{params.TargetNode},
									},
								},
							},
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "iperf3",
					Image: "mlabbe/iperf3:3.16-r0",
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
