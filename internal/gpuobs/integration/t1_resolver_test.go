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
//
// 이름 / 라벨 값들은 모두 RFC 1123 subdomain 규칙 (lowercase 알파벳 / 숫자 / 하이픈) 을 따른다.
// kube-apiserver 가 spec.nodeName / metadata.name 등에 본 규칙을 강제하기 때문이다.
func TestT1_ResolverIndexConsistency(t *testing.T) {
	cs, _, stop := startEnvtest(t)
	defer stop()

	r := kube.NewResolverWithClient(cs, "node-a", 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Start(ctx)

	// informer 의 초기 sync 가 끝날 때까지 대기. cold CI runner 에서는 envtest boot + apiserver 응답 +
	// informer list-watch 까지 합쳐 수 초가 걸릴 수 있어 ceiling 을 10 초로 둔다.
	if !waitFor(t, 10*time.Second, r.HasSynced) {
		t.Fatal("Resolver.HasSynced did not become true within 10s")
	}

	// envtest 의 in-process apiserver 는 namespace 를 자동 생성하지 않는다. Pod 생성 전에 ml 네임스페이스를
	// 명시적으로 만든다.
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ml"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Pod 생성: ResolveIP 가 PodIdentity 를 반환해야 한다. UID 는 apiserver 가 자동 할당하므로 명시
	// 하지 않는다 (assertion 도 PodName 만 검증).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "trainer-0", Namespace: "ml"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "main", Image: "img"}},
		},
	}
	created, err := cs.CoreV1().Pods("ml").Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}
	// envtest 는 status subresource 를 별도로 갱신해야 PodIP 가 반영된다.
	created.Status.PodIP = "10.0.0.5"
	if _, err := cs.CoreV1().Pods("ml").UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	if !waitFor(t, 5*time.Second, func() bool {
		id := r.ResolveIP("10.0.0.5")
		return id.IsPod() && id.PodName == "trainer-0"
	}) {
		t.Fatalf("ResolveIP(10.0.0.5) did not become trainer-0 within 5s; got %+v", r.ResolveIP("10.0.0.5"))
	}

	// Service 생성: ClusterIP 를 envtest 의 기본 service CIDR (10.0.0.0/24) 안에서 명시한다.
	// 자동 할당으로 두면 IP 가 동적이라 assertion 측에서 한 번 더 read 해야 하므로 명시 IP 가 더
	// 단순하다. ports 는 ServiceSpec 검증에서 빈 슬라이스가 거부될 수 있어 한 항목 둔다.
	svcClusterIP := "10.0.0.50"
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ml"},
		Spec: corev1.ServiceSpec{
			ClusterIP: svcClusterIP,
			Ports:     []corev1.ServicePort{{Port: 80}},
		},
	}
	if _, err := cs.CoreV1().Services("ml").Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		id := r.ResolveIP(svcClusterIP)
		return id.IsService() && id.Workload == "api"
	}) {
		t.Fatalf("ResolveIP(%s) did not become svc api within 5s; got %+v", svcClusterIP, r.ResolveIP(svcClusterIP))
	}

	// Node 생성: ResolveIP 가 NodeIdentity 를 반환해야 한다. node 이름도 RFC 1123 lowercase.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
	}
	createdNode, err := cs.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	createdNode.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.168.1.20"}}
	if _, err := cs.CoreV1().Nodes().UpdateStatus(ctx, createdNode, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update node status: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		id := r.ResolveIP("192.168.1.20")
		return id.IsNode() && id.NodeName == "node-b"
	}) {
		t.Fatalf("ResolveIP(192.168.1.20) did not become node-b within 5s; got %+v", r.ResolveIP("192.168.1.20"))
	}

	// Pod 삭제 후 인덱스에서 자연 제거되는지 검증: stale 시리즈 누수의 통합 가드.
	// envtest 의 apiserver 는 default grace period (30s) 를 적용해 Delete 호출 직후에는 Pod 가
	// "Terminating" 상태로 남는다. 통합 테스트는 grace 를 기다릴 이유가 없으므로 GracePeriodSeconds=0
	// 으로 즉시 삭제해 informer DeleteFunc 를 곧바로 발화시킨다.
	zero := int64(0)
	if err := cs.CoreV1().Pods("ml").Delete(ctx, "trainer-0", metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	if !waitFor(t, 5*time.Second, func() bool {
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
