//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"netobs/internal/kube"
)

// TestT1_ResolverIndexConsistency 는 envtest 의 in-process kube-apiserver 에 Pod / Service / Node
// 객체를 CRUD 했을 때 kube.Resolver 의 IP / UID 인덱스가 informer 콜백을 통해 정합 갱신되는지
// 검증한다. 단위 테스트는 informer 콜백을 mock 하므로 실제 watch 이벤트의 ordering 까지는 잡지
// 못하는데, 본 테스트가 그 부분을 cover 한다.
func TestT1_ResolverIndexConsistency(t *testing.T) {
	cs, _, stop := startEnvtest(t)
	defer stop()

	r := kube.NewResolverWithClient(cs, "node-A", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Start(ctx)

	// informer 의 초기 sync 가 끝날 때까지 대기. 보통 envtest 에서는 1 초 미만이지만 ceiling 으로 5 초.
	if !waitFor(t, 5*time.Second, r.HasSynced) {
		t.Fatal("Resolver.HasSynced did not become true within 5s")
	}

	// envtest 의 in-process apiserver 는 namespace 를 자동 생성하지 않는다. Pod 생성 전에 ml 네임스페이스를
	// 명시적으로 만든다.
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ml"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Pod 생성: ResolveIP 가 PodIdentity 를 반환해야 한다.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "trainer-0", Namespace: "ml", UID: "uid-trainer-0"},
		Spec: corev1.PodSpec{
			NodeName: "node-A",
			Containers: []corev1.Container{{Name: "c", Image: "img"}},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.5"},
	}
	if _, err := cs.CoreV1().Pods("ml").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	// envtest 는 status subresource 를 별도로 갱신해야 PodIP 가 반영된다.
	pod.Status.PodIP = "10.0.0.5"
	if _, err := cs.CoreV1().Pods("ml").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	if !waitFor(t, 3*time.Second, func() bool {
		id := r.ResolveIP("10.0.0.5")
		return id.IsPod() && id.PodName == "trainer-0"
	}) {
		t.Fatalf("ResolveIP(10.0.0.5) did not become trainer-0 within 3s; got %+v", r.ResolveIP("10.0.0.5"))
	}

	// Service 생성: ResolveIP 가 Service identity 를 반환해야 한다.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ml"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.10"},
	}
	if _, err := cs.CoreV1().Services("ml").Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		id := r.ResolveIP("10.96.0.10")
		return id.IsService() && id.Workload == "api"
	}) {
		t.Fatalf("ResolveIP(10.96.0.10) did not become svc api within 3s; got %+v", r.ResolveIP("10.96.0.10"))
	}

	// Node 생성: ResolveIP 가 NodeIdentity 를 반환해야 한다.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-B"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.20"}},
		},
	}
	if _, err := cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	node.Status = corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.20"}},
	}
	if _, err := cs.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update node status: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		id := r.ResolveIP("192.168.1.20")
		return id.IsNode() && id.NodeName == "node-B"
	}) {
		t.Fatalf("ResolveIP(192.168.1.20) did not become node-B within 3s; got %+v", r.ResolveIP("192.168.1.20"))
	}

	// Pod 삭제 후 인덱스에서 자연 제거되는지 검증: stale 시리즈 누수의 통합 가드.
	if err := cs.CoreV1().Pods("ml").Delete(ctx, "trainer-0", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	if !waitFor(t, 3*time.Second, func() bool {
		return !r.ResolveIP("10.0.0.5").IsPod()
	}) {
		t.Fatalf("ResolveIP(10.0.0.5) still points to deleted pod; got %+v", r.ResolveIP("10.0.0.5"))
	}
}

// waitFor 는 cond 가 true 가 될 때까지 timeout 안에서 짧은 polling 으로 대기한다. 통합 테스트의
// eventually pattern 표준 헬퍼로 사용한다 (의도적 sleep 금지 방침과 정합).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}
