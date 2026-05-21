package safety

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"netobs/internal/injector/loadgen"
)

// TestCheckDuration 은 duration 가드의 boundary 를 검증한다.
func TestCheckDuration(t *testing.T) {
	cases := []struct {
		d       time.Duration
		wantErr bool
	}{
		{0, true},
		{-1 * time.Second, true},
		{1 * time.Second, false},
		{30 * time.Minute, false},
		{31 * time.Minute, true},
	}
	for _, tc := range cases {
		err := CheckDuration(tc.d)
		if (err != nil) != tc.wantErr {
			t.Errorf("d=%s err=%v want_err=%v", tc.d, err, tc.wantErr)
		}
	}
}

// TestCheckIntensity 는 kind 별 상한이 정확히 boundary 에서 동작하는지 검증한다.
func TestCheckIntensity(t *testing.T) {
	cases := []struct {
		kind    loadgen.Kind
		input   string
		wantErr bool
	}{
		// cpu
		{loadgen.KindCPU, "500m", false},
		{loadgen.KindCPU, "4000m", false},
		{loadgen.KindCPU, "4001m", true},
		{loadgen.KindCPU, "5", true}, // 5 core = 5000m
		{loadgen.KindCPU, "not-a-number", true},
		// network. 1G == 1000M 라 boundary 통과, 2G 부터 거부.
		{loadgen.KindNetwork, "100M", false},
		{loadgen.KindNetwork, "1000M", false},
		{loadgen.KindNetwork, "1G", false},
		{loadgen.KindNetwork, "2G", true},
		{loadgen.KindNetwork, "abc", true},
		// gpu
		{loadgen.KindGPU, "1", false},
		{loadgen.KindGPU, "2", true},
		{loadgen.KindGPU, "", true},
		// memory. 2G binary == 2 << 30 bytes 가 상한.
		{loadgen.KindMemory, "512M", false},
		{loadgen.KindMemory, "1G", false},
		{loadgen.KindMemory, "2G", false},
		{loadgen.KindMemory, "3G", true},
		{loadgen.KindMemory, "abc", true},
		{loadgen.KindMemory, "", true},
	}
	for _, tc := range cases {
		err := CheckIntensity(tc.kind, tc.input)
		if (err != nil) != tc.wantErr {
			t.Errorf("kind=%s input=%s err=%v want_err=%v", tc.kind, tc.input, err, tc.wantErr)
		}
	}
}

// TestCheckClusterLabel_NoMatchingNode 는 매칭 노드가 없을 때 fail-fast 되는지 검증한다.
func TestCheckClusterLabel_NoMatchingNode(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-node-1", Labels: map[string]string{"environment": "prod"}},
	})
	err := CheckClusterLabel(context.Background(), client, "environment=dev")
	if err == nil || !strings.Contains(err.Error(), "no node matches") {
		t.Errorf("err=%v want no-match error", err)
	}
}

// TestCheckClusterLabel_MatchingNode 는 매칭 노드 1 개 이상이면 통과하는지 검증한다.
func TestCheckClusterLabel_MatchingNode(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-node-1", Labels: map[string]string{"environment": "dev"}},
	})
	if err := CheckClusterLabel(context.Background(), client, "environment=dev"); err != nil {
		t.Errorf("err=%v want nil", err)
	}
}

// TestCheckClusterLabel_EmptyRequiredSkips 는 빈 라벨이 검사 skip 으로 통과되는지 검증한다.
func TestCheckClusterLabel_EmptyRequiredSkips(t *testing.T) {
	client := fake.NewSimpleClientset()
	if err := CheckClusterLabel(context.Background(), client, ""); err != nil {
		t.Errorf("err=%v want nil (empty label skips)", err)
	}
}

// TestAcquireLock_FirstAcquireSucceeds 는 lease 가 없는 상태에서 lock 획득에 성공하는지 검증한다.
func TestAcquireLock_FirstAcquireSucceeds(t *testing.T) {
	client := fake.NewSimpleClientset()
	release, err := AcquireLock(context.Background(), client, "ebpf-project", "default", "victim", "test-holder", 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if release == nil {
		t.Fatalf("release is nil")
	}
	release()
}

// TestAcquireLock_ConcurrentHolderRejected 는 같은 target 에 두 번째 lock 획득이 거부되는지 검증한다.
func TestAcquireLock_ConcurrentHolderRejected(t *testing.T) {
	client := fake.NewSimpleClientset()
	if _, err := AcquireLock(context.Background(), client, "ebpf-project", "default", "victim", "h1", 5*time.Minute); err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	_, err := AcquireLock(context.Background(), client, "ebpf-project", "default", "victim", "h2", 5*time.Minute)
	if err == nil {
		t.Errorf("err=nil want concurrent rejection")
	}
}

// TestAcquireLock_ExpiredLeaseStealable 은 TTL 만료된 lease 를 새 holder 가 강제 갱신 가능한지 검증한다.
func TestAcquireLock_ExpiredLeaseStealable(t *testing.T) {
	client := fake.NewSimpleClientset()
	if _, err := AcquireLock(context.Background(), client, "ebpf-project", "default", "victim", "h1", -1*time.Minute); err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if _, err := AcquireLock(context.Background(), client, "ebpf-project", "default", "victim", "h2", 5*time.Minute); err != nil {
		t.Errorf("steal expired lease: %v", err)
	}
}

// TestLockName_DifferentTargetsIsolated 는 다른 target 의 lock 이 별도로 분리되는지 검증한다.
func TestLockName_DifferentTargetsIsolated(t *testing.T) {
	a := LockName("ns1", "pod1")
	b := LockName("ns2", "pod1")
	c := LockName("ns1", "pod2")
	if a == b || a == c || b == c {
		t.Errorf("lock names collide: %s %s %s", a, b, c)
	}
}
