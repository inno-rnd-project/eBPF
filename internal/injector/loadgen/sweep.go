package loadgen

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// stressPodSelector 는 본 패키지가 spawn 하는 전 부하 pod 의 공통 라벨 셀렉터다 (commonPodMeta 와
// 정합). sweep 의 회수 대상 식별에 사용된다.
const stressPodSelector = "app.kubernetes.io/name=workload-injector,app.kubernetes.io/component=stress"

// SweepOrphans 는 컨트롤러 기동 시 이전 인스턴스가 남긴 고아 부하 pod 를 회수한다 (#418). 대상은
// 두 부류다.
//
//   - terminal phase (Succeeded / Failed): 자체 종료나 activeDeadlineSeconds 로 이미 끝났지만
//     RestartPolicy Never pod 객체가 남아 있는 경우
//   - age 가 maxDuration + deadline 여유를 넘긴 pod: 어떤 정상 run 도 maxDuration (호출자가
//     safety.DurationLimit 을 전달, import cycle 회피용 파라미터) 을 넘길 수 없으므로 확실한 고아
//
// 재시작 후 재개되는 Running 시나리오의 pod 는 age 가 maxDuration 이하라 건드리지 않는다.
// namespace 는 빈 문자열이면 전 namespace 를 훑는다 (LoadScenario 가 CR namespace 에 spawn 하므로).
func SweepOrphans(ctx context.Context, client kubernetes.Interface, namespace string, maxDuration time.Duration) (int, error) {
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: stressPodSelector})
	if err != nil {
		return 0, fmt.Errorf("sweep list: %w", err)
	}
	maxAge := maxDuration + deadlineGraceSec*time.Second
	now := time.Now()
	swept := 0
	var lastErr error
	for _, p := range pods.Items {
		terminal := p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
		expired := now.Sub(p.CreationTimestamp.Time) > maxAge
		if !terminal && !expired {
			continue
		}
		if err := deletePod(ctx, client, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}); err != nil {
			lastErr = err
			continue
		}
		swept++
	}
	return swept, lastErr
}
