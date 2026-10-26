package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	injectorv1alpha1 "netobs/api/v1alpha1"
)

// useActiveCountReader 는 ActiveCount 의 집계 reader 를 테스트 client 로 바꾸고 종료 시 되돌린다.
func useActiveCountReader(t *testing.T, c client.Reader) {
	t.Helper()
	setActiveCountReader(c)
	t.Cleanup(func() { activeCountReader.Store(nil) })
}

func cronScenario(name string, state injectorv1alpha1.LoadScenarioRunState) *injectorv1alpha1.LoadScenario {
	ls := scoreTriggerScenario()
	ls.Name = name
	ls.Spec.ScoreTrigger = nil
	ls.Status.RunState = state
	return ls
}

// TestActiveCount_CountsPersistedRunningOnly 는 #459 의 집계 기준을 검증한다. 게이지는 영속된
// RunState=Running CR 만 센다.
func TestActiveCount_CountsPersistedRunningOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = injectorv1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		cronScenario("a", injectorv1alpha1.RunStateRunning),
		cronScenario("b", injectorv1alpha1.RunStateRunning),
		cronScenario("c", injectorv1alpha1.RunStateIdle),
		cronScenario("d", injectorv1alpha1.RunStateAwaitingSpikeAlert),
	).Build()
	useActiveCountReader(t, c)

	if got := testutil.ToFloat64(ActiveCount); got != 2 {
		t.Errorf("active=%v want 2 (Running 만)", got)
	}
}

// TestActiveCount_StatusUpdateConflictDoesNotInflate 는 Running 전이의 status update 가 충돌로 실패하면
// 게이지가 오르지 않는지 검증한다. 종전에는 Inc 가 update 보다 먼저라 RunState 는 Idle 로 남는데
// 게이지만 1 올라 상향 드리프트했다.
func TestActiveCount_StatusUpdateConflictDoesNotInflate(t *testing.T) {
	ls := cronScenario("conflict", "")
	r, _ := newScoreReconciler(ls, nil, time.Now())
	scheme := runtime.NewScheme()
	_ = injectorv1alpha1.AddToScheme(scheme)
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ls).WithStatusSubresource(ls).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "injector.netobs.io", Resource: "loadscenarios"}, ls.Name, nil)
			},
		}).Build()
	useActiveCountReader(t, r.Client)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}}); err == nil {
		t.Fatalf("Reconcile err=nil, want status update conflict")
	}
	if got := testutil.ToFloat64(ActiveCount); got != 0 {
		t.Errorf("active=%v want 0 (영속 실패한 Running 전이는 세지 않음)", got)
	}
}

// TestActiveCount_FollowsRunLifecycleAndDeletion 은 run 시작, 실행 중 삭제 시 게이지가 상태를 따라가는지
// 검증한다. 종전에는 Running 상태 CR 삭제 시 감산이 없어 게이지가 영구히 1 로 남았다.
func TestActiveCount_FollowsRunLifecycleAndDeletion(t *testing.T) {
	ls := cronScenario("lifecycle", "")
	r, c := newScoreReconciler(ls, nil, time.Now())
	useActiveCountReader(t, c)
	ctx := context.Background()

	reconcileOnce(t, r, ls)
	if got := getScenario(t, c, ls).Status.RunState; got != injectorv1alpha1.RunStateRunning {
		t.Fatalf("runState=%q want Running", got)
	}
	if got := testutil.ToFloat64(ActiveCount); got != 1 {
		t.Fatalf("run 시작 후 active=%v want 1", got)
	}

	// 실행 중 삭제: finalizer 정리 후 CR 이 사라지면 게이지에서 빠진다.
	if err := c.Delete(ctx, getScenario(t, c, ls)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	reconcileOnce(t, r, ls)
	var gone injectorv1alpha1.LoadScenario
	if err := c.Get(ctx, types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("삭제 후 CR 잔존 err=%v", err)
	}
	if got := testutil.ToFloat64(ActiveCount); got != 0 {
		t.Errorf("실행 중 삭제 후 active=%v want 0", got)
	}
}

// TestActiveCount_RunEndRetriedTransitionNoDoubleDecrement 는 run 종료 전이의 status update 가 한 번
// 실패한 뒤 재시도되어도 게이지가 정확한지 검증한다. 종전에는 Dec 후 전이 update 가 실패하면 재시도
// reconcile 이 다시 Dec 해 이중 감산 (음수) 이 됐다. 실패 동안은 영속 상태가 Running 이라 1 이 맞고,
// 재시도 성공 후 0 이 된다.
func TestActiveCount_RunEndRetriedTransitionNoDoubleDecrement(t *testing.T) {
	start := time.Now()
	ls := cronScenario("ending", injectorv1alpha1.RunStateRunning)
	runStart := metav1.NewTime(start.Add(-2 * time.Minute))
	ls.Status.RunStartTime = &runStart
	r, _ := newScoreReconciler(ls, nil, start)
	scheme := runtime.NewScheme()
	_ = injectorv1alpha1.AddToScheme(scheme)
	failures := 1
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ls).WithStatusSubresource(ls).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if failures > 0 {
					failures--
					return apierrors.NewConflict(schema.GroupResource{Group: "injector.netobs.io", Resource: "loadscenarios"}, obj.GetName(), nil)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	useActiveCountReader(t, r.Client)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}}

	if got := testutil.ToFloat64(ActiveCount); got != 1 {
		t.Fatalf("종료 전 active=%v want 1", got)
	}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatalf("첫 종료 reconcile err=nil, want 주입한 conflict")
	}
	if got := testutil.ToFloat64(ActiveCount); got != 1 {
		t.Errorf("전이 실패 후 active=%v want 1 (영속 상태 Running)", got)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("재시도 reconcile err=%v", err)
	}
	if got := testutil.ToFloat64(ActiveCount); got != 0 {
		t.Errorf("재시도 성공 후 active=%v want 0 (이중 감산 없음)", got)
	}
}
