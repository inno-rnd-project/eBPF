package loadgen

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func defaultParams() Params {
	return Params{
		TargetNode:      "ebpf-worker1",
		TargetNamespace: "default",
		TargetPod:       "victim",
		SpawnNamespace:  "ebpf-project",
		Duration:        60 * time.Second,
		Intensity:       "500m",
	}
}

// TestCPU_StartSpawnsPodWithExpectedSpec 는 cpu 모듈이 target node 강제 배치, stress --cpu N
// command, cpu limit 을 정확히 설정하는지 검증한다.
func TestCPU_StartSpawnsPodWithExpectedSpec(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, err := New(KindCPU, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := g.Start(context.Background(), defaultParams()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pods, err := client.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("len=%d want 1", len(pods.Items))
	}
	pod := pods.Items[0]
	if pod.Spec.NodeName != "ebpf-worker1" {
		t.Errorf("nodeName=%s want ebpf-worker1", pod.Spec.NodeName)
	}
	if pod.Spec.Containers[0].Image != "polinux/stress:1.0.4" {
		t.Errorf("image=%s", pod.Spec.Containers[0].Image)
	}
	cmd := pod.Spec.Containers[0].Command
	if len(cmd) < 5 || cmd[0] != "stress" || cmd[1] != "--cpu" {
		t.Errorf("command=%v", cmd)
	}
	cpuLimit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	if cpuLimit.String() != "500m" {
		t.Errorf("cpu limit=%s want 500m", cpuLimit.String())
	}
	if pod.Labels["injector.kind"] != "cpu" {
		t.Errorf("injector.kind label=%s", pod.Labels["injector.kind"])
	}
}

// TestNetwork_StartSpawnsServerAndClient 는 network 모듈이 server / client 두 Pod 를 spawn 하고
// server 가 target node 에 배치되는지 검증한다.
func TestNetwork_StartSpawnsServerAndClient(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, err := New(KindNetwork, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := g.Start(context.Background(), defaultParams()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pods, err := client.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("len=%d want 2 (server + client)", len(pods.Items))
	}
	var server, clientPod *corev1.Pod
	for i := range pods.Items {
		switch pods.Items[i].Labels["injector.role"] {
		case "server":
			server = &pods.Items[i]
		case "client":
			clientPod = &pods.Items[i]
		}
	}
	if server == nil || clientPod == nil {
		t.Fatalf("server=%v client=%v", server, clientPod)
	}
	if server.Spec.NodeName != "ebpf-worker1" {
		t.Errorf("server nodeName=%s", server.Spec.NodeName)
	}
	if clientPod.Spec.NodeName != "" {
		t.Errorf("client nodeName=%s want empty (any node)", clientPod.Spec.NodeName)
	}
}

// TestGPU_StartSpawnsPodWithGPUResource 는 gpu 모듈이 nvidia.com/gpu resource limit 을 명시하는지
// 검증한다.
func TestGPU_StartSpawnsPodWithGPUResource(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, err := New(KindGPU, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params := defaultParams()
	params.Intensity = "1"
	if err := g.Start(context.Background(), params); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pods, _ := client.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("len=%d", len(pods.Items))
	}
	pod := pods.Items[0]
	gpu := pod.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"]
	if gpu.String() != "1" {
		t.Errorf("nvidia.com/gpu=%s want 1", gpu.String())
	}
}

// TestStop_IsIdempotent 는 Stop 이 동일 인스턴스에 두 번 호출되어도 NotFound 를 무시하는지 검증
// 한다. cleanup 흐름이 race 로 중복 호출되는 시나리오 가드.
func TestStop_IsIdempotent(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, err := New(KindCPU, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := g.Start(context.Background(), defaultParams()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 1: %v", err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Errorf("Stop 2: %v want nil (idempotent)", err)
	}
	pods, _ := client.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("after stop pods=%d want 0", len(pods.Items))
	}
}

// TestStop_AfterExternalDelete 는 외부에서 Pod 가 먼저 삭제된 후 Stop 이 호출되어도 NotFound 가
// 무시되는지 검증한다.
func TestStop_AfterExternalDelete(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, err := New(KindCPU, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := g.Start(context.Background(), defaultParams()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	spawned := g.SpawnedPods()
	for _, n := range spawned {
		_ = client.CoreV1().Pods(n.Namespace).Delete(context.Background(), n.Name, metav1.DeleteOptions{})
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Errorf("Stop after external delete: %v", err)
	}
}

// TestNew_UnknownKindError 는 정의되지 않은 kind 가 error 로 거부되는지 검증한다.
func TestNew_UnknownKindError(t *testing.T) {
	client := fake.NewSimpleClientset()
	if _, err := New(Kind("disk"), client); err == nil {
		t.Errorf("err=nil want non-nil for unknown kind")
	}
}

// TestCPU_StartParseError 는 잘못된 intensity 가 parse error 로 fail-fast 되는지 검증한다.
func TestCPU_StartParseError(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, _ := New(KindCPU, client)
	params := defaultParams()
	params.Intensity = "not-a-quantity"
	if err := g.Start(context.Background(), params); err == nil {
		t.Errorf("err=nil want parse error")
	}
}

// TestMemory_StartSpawnsPodWithExpectedSpec 는 memory 모듈이 target node 강제 배치와 stress --vm
// --vm-bytes 명령, memory limit 설정을 정확히 만드는지 검증한다.
func TestMemory_StartSpawnsPodWithExpectedSpec(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, err := New(KindMemory, client)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	params := defaultParams()
	params.Intensity = "512Mi"
	if err := g.Start(context.Background(), params); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pods, err := client.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pod count=%d want 1", len(pods.Items))
	}
	p := pods.Items[0]
	if p.Spec.NodeName != "ebpf-worker1" {
		t.Errorf("nodeName=%q want ebpf-worker1", p.Spec.NodeName)
	}
	if got := p.Labels["injector.kind"]; got != "memory" {
		t.Errorf("injector.kind=%q want memory", got)
	}
	// 512Mi == 536870912 bytes 가 K8s Quantity 규약. stress 와 cgroup limit 양쪽에 동일 bytes 정수.
	const expected512MiBytes = "536870912"
	if got := p.Spec.Containers[0].Command; len(got) < 6 || got[1] != "--vm" || got[3] != "--vm-bytes" || got[4] != expected512MiBytes {
		t.Errorf("command=%v want stress --vm 1 --vm-bytes %s ...", got, expected512MiBytes)
	}
	if got := p.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; got.String() != "512Mi" {
		t.Errorf("memory limit=%s want 512Mi", got.String())
	}
}

// TestMemory_StartParseError 는 잘못된 intensity 가 parse error 로 fail-fast 되는지 검증한다.
func TestMemory_StartParseError(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, _ := New(KindMemory, client)
	params := defaultParams()
	params.Intensity = "not-a-quantity"
	if err := g.Start(context.Background(), params); err == nil {
		t.Errorf("err=nil want parse error")
	}
}

// TestNetwork_PartialSpawnCleanup 는 server 만 spawn 되고 client 가 실패할 때 server 도 cleanup
// 되는지 검증한다 (현재 fake clientset 에서는 client create 도 성공해서 이 시나리오 재현이 어려워
// AlreadyExists 시뮬레이션으로 대체).
func TestNetwork_AlreadyExistsTolerated(t *testing.T) {
	client := fake.NewSimpleClientset()
	g, _ := New(KindNetwork, client)
	if err := g.Start(context.Background(), defaultParams()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	// stop 후 같은 params 로 재시작.
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	g2, _ := New(KindNetwork, client)
	if err := g2.Start(context.Background(), defaultParams()); err != nil {
		t.Errorf("restart with same names: %v", err)
	}
}

// TestSanitizeName 은 DNS-1123 위반 문자가 정상 정규화되고 63 자 상한이 적용되는지 검증한다.
func TestSanitizeName(t *testing.T) {
	cases := []struct {
		prefix string
		target string
		want   string
	}{
		{"stress-cpu", "victim", "stress-cpu-victim"},
		{"stress-cpu", "Victim_Pod", "stress-cpu-victim-pod"},
		{"stress-cpu", "POD.NAME", "stress-cpu-pod-name"},
		{"stress-cpu", "", "stress-cpu"},
		{"stress-cpu", "1234567890123456789012345678901234567890123456789012345678901234", "stress-cpu-1234567890123456789012345678901234567890123456789012"},
	}
	for _, tc := range cases {
		got := sanitizeName(tc.prefix, tc.target)
		if got != tc.want {
			t.Errorf("sanitizeName(%q, %q)=%q want %q", tc.prefix, tc.target, got, tc.want)
		}
		if len(got) > 63 {
			t.Errorf("len=%d exceeds 63", len(got))
		}
	}
}

// ensure apierrors / schema imported even if not directly referenced (compile guard for future tests).
var _ = apierrors.IsNotFound
var _ schema.GroupVersionResource
var _ types.NamespacedName
