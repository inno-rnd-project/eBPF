package loadgen

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestSweepOrphans 는 #418 기동 sweep 의 세 분기를 단정한다. terminal phase 와 상한 초과 age 는
// 회수되고, 진행 중일 수 있는 젊은 Running pod 와 stress 라벨이 아닌 pod 는 보존된다.
func TestSweepOrphans(t *testing.T) {
	maxDuration := 30 * time.Minute
	stressLabels := map[string]string{
		"app.kubernetes.io/name":      "workload-injector",
		"app.kubernetes.io/component": "stress",
	}
	mkPod := func(name string, labels map[string]string, phase corev1.PodPhase, age time.Duration) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "ebpf-project", Labels: labels,
				CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			},
			Status: corev1.PodStatus{Phase: phase},
		}
	}
	client := fake.NewSimpleClientset(
		mkPod("done", stressLabels, corev1.PodSucceeded, time.Minute),
		mkPod("crashed", stressLabels, corev1.PodFailed, time.Minute),
		mkPod("ancient", stressLabels, corev1.PodRunning, time.Hour),
		mkPod("young-running", stressLabels, corev1.PodRunning, time.Minute),
		mkPod("unrelated", map[string]string{"app": "other"}, corev1.PodRunning, time.Hour),
	)

	swept, err := SweepOrphans(context.Background(), client, "", maxDuration)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 3 {
		t.Errorf("swept=%d want 3", swept)
	}
	remaining, _ := client.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{})
	names := map[string]bool{}
	for _, p := range remaining.Items {
		names[p.Name] = true
	}
	if !names["young-running"] || !names["unrelated"] {
		t.Errorf("보존 대상이 삭제됨: remaining=%v", names)
	}
	if names["done"] || names["crashed"] || names["ancient"] {
		t.Errorf("회수 대상이 남음: remaining=%v", names)
	}
}
