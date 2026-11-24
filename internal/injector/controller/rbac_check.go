package controller

import (
	"context"
	"log"
	"strings"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// rbacCheckVerbs는 컨트롤러가 허용 namespace에서 pods에 쓰는 verb다. create는 부하 pod 생성
// (loadgen.createPod), delete는 정리(cleanupStressPods, deletePod), get은 대상 pod 조회, list는 정리와
// 기동 orphan sweep이 쓴다. configmaps와 leases는 lock namespace에서만 쓰므로 검사하지 않는다.
var rbacCheckVerbs = []string{"create", "delete", "get", "list"}

const (
	// rbacCheckInterval은 재검사 주기다. Role은 기동 뒤에 추가하는 것이 곧 조치라 주기적으로 다시 봐야
	// 재시작 없이 상태가 풀린다. alert의 for(15m)보다 짧아 일시적 거부는 발화 전에 한 번 재검사된다.
	rbacCheckInterval = 10 * time.Minute
	// rbacReviewTimeout은 review 한 건의 상한이다. API server가 느려도 검사 한 바퀴가 주기를 넘지 않는다.
	rbacReviewTimeout = 5 * time.Second
)

// NamespaceRBACChecker는 INJECTOR_ALLOWED_TARGET_NAMESPACES의 namespace마다 컨트롤러 ServiceAccount가
// pods 권한을 실제로 갖는지 SelfSubjectAccessReview로 확인한다(#420 리뷰의 후속). 허용 목록에
// namespace를 넣고 Role과 RoleBinding을 빠뜨리면 게이트는 통과하고 pod 생성 시점에야 forbidden으로
// 실패해 원인 추적이 멀어지므로, 불일치를 gauge와 로그로 먼저 드러낸다.
//
// 검사는 진단용이라 fail open이다. 거부나 review 실패가 기동, reconcile, readiness를 막지 않는다.
// SelfSubjectAccessReview 생성은 system:basic-user가 모든 인증 주체에 허용하므로 RBAC 추가가 필요 없다.
type NamespaceRBACChecker struct {
	Client     kubernetes.Interface
	Namespaces []string

	// last는 namespace별 직전 상태다. 상태가 바뀔 때만 로그를 남겨 10분마다 같은 줄이 쌓이지 않게 한다.
	last map[string]string
	logf func(format string, args ...any)
}

// NeedLeaderElection은 false다. manager가 leader 선출 전에 시작해 lease를 기다리는 replica도 검사하며,
// 검사는 읽기 전용이라 여러 replica가 동시에 돌아도 안전하다.
func (c *NamespaceRBACChecker) NeedLeaderElection() bool { return false }

// Start는 즉시 한 번 검사하고 ctx가 끝날 때까지 주기적으로 반복한다. runnable이 error를 돌려주면
// manager 전체가 멈추므로 항상 nil을 돌려준다.
func (c *NamespaceRBACChecker) Start(ctx context.Context) error {
	wait.UntilWithContext(ctx, c.RunOnce, rbacCheckInterval)
	return nil
}

// RunOnce는 허용 namespace 전부를 한 바퀴 검사한다. 허용이면 gauge 1, 거부면 0을 쓰고, review 호출이
// 실패하면 gauge를 직전 값으로 두고 counter의 error만 올린다. 존재하지 않는 namespace는 RBAC가 Role
// 누락과 똑같이 거부로 답한다.
func (c *NamespaceRBACChecker) RunOnce(ctx context.Context) {
	logf := c.logf
	if logf == nil {
		logf = log.Printf
	}
	first := c.last == nil
	if first {
		c.last = map[string]string{}
	}
	var allowed, denied, failed int
	for _, ns := range c.Namespaces {
		var deniedVerbs []string
		var reviewErr error
		for _, verb := range rbacCheckVerbs {
			if ctx.Err() != nil {
				return // 종료 중의 취소는 review 실패로 세지 않는다.
			}
			rctx, cancel := context.WithTimeout(ctx, rbacReviewTimeout)
			res, err := c.Client.AuthorizationV1().SelfSubjectAccessReviews().Create(rctx, &authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Resource: "pods"},
				},
			}, metav1.CreateOptions{})
			cancel()
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return
				}
				failed++
				NamespaceRBACCheckTotal.WithLabelValues("error").Inc()
				reviewErr = err
			case res.Status.Allowed:
				allowed++
				NamespaceRBACCheckTotal.WithLabelValues("allowed").Inc()
				NamespaceRBACAllowed.WithLabelValues(ns, verb).Set(1)
			default:
				denied++
				NamespaceRBACCheckTotal.WithLabelValues("denied").Inc()
				NamespaceRBACAllowed.WithLabelValues(ns, verb).Set(0)
				deniedVerbs = append(deniedVerbs, verb)
			}
		}

		state := "allowed"
		switch {
		case len(deniedVerbs) > 0:
			state = "denied:" + strings.Join(deniedVerbs, ",")
		case reviewErr != nil:
			state = "error"
		}
		prev, seen := c.last[ns]
		if state == prev {
			continue
		}
		c.last[ns] = state
		switch {
		case len(deniedVerbs) > 0:
			logf("controller: rbac check ns=%s denied verbs=%s resource=pods: listed in INJECTOR_ALLOWED_TARGET_NAMESPACES but no Role/RoleBinding grants these verbs to this ServiceAccount, or the namespace does not exist; runs that spawn in or target it fail with forbidden", ns, strings.Join(deniedVerbs, ","))
		case reviewErr != nil:
			logf("controller: rbac check ns=%s review failed: %v (fail open, keeping last state, retry in %s)", ns, reviewErr, rbacCheckInterval)
		case seen:
			logf("controller: rbac check ns=%s allowed again", ns)
		}
	}
	if first {
		logf("controller: rbac check namespaces=%d verbs=%s allowed=%d denied=%d error=%d", len(c.Namespaces), strings.Join(rbacCheckVerbs, ","), allowed, denied, failed)
	}
}
