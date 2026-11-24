package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// reviewClient는 SelfSubjectAccessReview에 decide의 판정으로 답하는 fake clientset이다. reactor 없이
// 쓰면 fake tracker가 첫 호출에는 allowed=false를, 둘째 호출에는 AlreadyExists를 돌려주므로 반드시
// reactor를 앞에 붙인다. 받은 review는 seen에 기록해 요청 속성을 검증한다.
func reviewClient(decide func(ns, verb string) (bool, error), seen *[]authorizationv1.ResourceAttributes) *fake.Clientset {
	c := fake.NewSimpleClientset()
	c.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		if seen != nil {
			*seen = append(*seen, *r.Spec.ResourceAttributes)
		}
		ok, err := decide(r.Spec.ResourceAttributes.Namespace, r.Spec.ResourceAttributes.Verb)
		if err != nil {
			return true, nil, err
		}
		r.Status.Allowed = ok
		return true, r, nil
	})
	return c
}

func gauge(ns, verb string) float64 {
	return testutil.ToFloat64(NamespaceRBACAllowed.WithLabelValues(ns, verb))
}

func total(result string) float64 {
	return testutil.ToFloat64(NamespaceRBACCheckTotal.WithLabelValues(result))
}

// TestNamespaceRBACChecker_Lifecycle은 한 checker로 상태 전이를 차례로 밟는다. Role이 일부만 있는
// namespace는 빠진 verb만 0이 되고, review 실패는 gauge를 직전 값으로 둔 채 counter의 error만 올리며,
// Role을 추가하면 재시작 없이 1로 돌아온다. 로그는 namespace 상태가 바뀔 때만 한 줄 남는다.
func TestNamespaceRBACChecker_Lifecycle(t *testing.T) {
	NamespaceRBACAllowed.Reset()
	NamespaceRBACCheckTotal.Reset()
	mode := "partial"
	var seen []authorizationv1.ResourceAttributes
	var logs []string
	c := &NamespaceRBACChecker{
		Namespaces: []string{"ok", "bad"},
		logf:       func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
		Client: reviewClient(func(ns, verb string) (bool, error) {
			switch mode {
			case "partial": // bad는 create와 get만 허용
				return ns == "ok" || verb == "create" || verb == "get", nil
			case "error":
				return false, errors.New("apiserver unavailable")
			default:
				return true, nil
			}
		}, &seen),
	}

	c.RunOnce(context.Background())
	if len(seen) != 8 {
		t.Fatalf("reviews=%d want 8 (2 namespace x 4 verb)", len(seen))
	}
	for _, ra := range seen {
		if ra.Resource != "pods" || ra.Group != "" || ra.Namespace == "" {
			t.Errorf("review 속성=%+v want core pods와 namespace 지정", ra)
		}
	}
	for _, verb := range rbacCheckVerbs {
		if gauge("ok", verb) != 1 {
			t.Errorf("ok/%s=%v want 1", verb, gauge("ok", verb))
		}
	}
	if gauge("bad", "create") != 1 || gauge("bad", "get") != 1 || gauge("bad", "delete") != 0 || gauge("bad", "list") != 0 {
		t.Errorf("bad gauge create=%v get=%v delete=%v list=%v want 1/1/0/0", gauge("bad", "create"), gauge("bad", "get"), gauge("bad", "delete"), gauge("bad", "list"))
	}
	if total("allowed") != 6 || total("denied") != 2 || total("error") != 0 {
		t.Errorf("counter allowed=%v denied=%v error=%v want 6/2/0", total("allowed"), total("denied"), total("error"))
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "ns=bad denied verbs=delete,list") || !strings.Contains(logs[1], "namespaces=2 verbs=create,delete,get,list allowed=6 denied=2 error=0") {
		t.Fatalf("첫 검사 로그=%q want bad 거부 한 줄과 요약 한 줄", logs)
	}

	// 같은 상태의 재검사는 로그를 남기지 않는다.
	logs = nil
	c.RunOnce(context.Background())
	if len(logs) != 0 {
		t.Errorf("상태 불변인데 로그=%q", logs)
	}

	// review 실패: gauge는 직전 값을 유지하고 error만 센다. 두 namespace 모두 한 줄씩, 반복해도 한 번만.
	mode = "error"
	c.RunOnce(context.Background())
	c.RunOnce(context.Background())
	if gauge("ok", "create") != 1 || gauge("bad", "delete") != 0 {
		t.Errorf("review 실패 뒤 gauge ok/create=%v bad/delete=%v want 직전 값 1/0", gauge("ok", "create"), gauge("bad", "delete"))
	}
	if total("error") != 16 {
		t.Errorf("error counter=%v want 16 (8 review x 2회)", total("error"))
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "ns=ok review failed: apiserver unavailable") || !strings.Contains(logs[1], "ns=bad review failed") {
		t.Errorf("review 실패 로그=%q want namespace당 한 줄", logs)
	}

	// Role 추가: 재시작 없이 풀린다.
	mode = "fixed"
	logs = nil
	c.RunOnce(context.Background())
	if gauge("bad", "delete") != 1 || gauge("bad", "list") != 1 {
		t.Errorf("복구 뒤 bad delete=%v list=%v want 1/1", gauge("bad", "delete"), gauge("bad", "list"))
	}
	if len(logs) != 2 || !strings.Contains(logs[0], "ns=ok allowed again") || !strings.Contains(logs[1], "ns=bad allowed again") {
		t.Errorf("복구 로그=%q want namespace당 allowed again", logs)
	}
}

// TestNamespaceRBACChecker_Runnable은 manager에 붙는 계약을 검증한다. leader 선출 없이 돌고, ctx가 이미
// 끝났으면 review를 하나도 보내지 않으며 취소를 review 실패로 세지 않고, Start는 nil을 돌려준다.
func TestNamespaceRBACChecker_Runnable(t *testing.T) {
	NamespaceRBACAllowed.Reset()
	NamespaceRBACCheckTotal.Reset()
	var seen []authorizationv1.ResourceAttributes
	c := &NamespaceRBACChecker{
		Namespaces: []string{"ok"},
		logf:       func(string, ...any) {},
		Client:     reviewClient(func(string, string) (bool, error) { return true, nil }, &seen),
	}
	if c.NeedLeaderElection() {
		t.Error("NeedLeaderElection=true want false")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Start(ctx); err != nil {
		t.Errorf("Start=%v want nil (error는 manager를 멈춘다)", err)
	}
	c.RunOnce(ctx)
	if len(seen) != 0 || total("error") != 0 {
		t.Errorf("취소된 ctx에서 review=%d error=%v want 0/0", len(seen), total("error"))
	}
}
