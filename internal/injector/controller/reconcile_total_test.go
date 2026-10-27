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
	"netobs/internal/injector/safety"
)

// reconcileResults는 reconcile_total의 result 열거 전체다. 모든 케이스에서 전 값의 증가분을 확인해
// 기대 결과 외의 값이 함께 오르는 이중 계수를 잡는다.
var reconcileResults = []string{"success", "error", "skip", "started", "wait", "denied", "suspended", "deleted", "retry"}

// reconcileTotal은 result 라벨별 reconcile_total 현재값이다.
func reconcileTotal(result string) float64 {
	return testutil.ToFloat64(ReconcileTotal.WithLabelValues(result))
}

// conflictOnStatusUpdate는 status update를 항상 충돌로 실패시키는 fake client를 만든다.
func conflictOnStatusUpdate(ls *injectorv1alpha1.LoadScenario) client.Client {
	scheme := runtime.NewScheme()
	_ = injectorv1alpha1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(ls).WithStatusSubresource(ls).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return apierrors.NewConflict(schema.GroupResource{Group: "injector.netobs.io", Resource: "loadscenarios"}, ls.Name, nil)
			},
		}).Build()
}

// TestReconcileTotal_CountsEveryReconcileOnce는 #462의 계수 정합을 검증한다. 존재하는 CR의 reconcile
// 마다 해당 result 하나만 정확히 1 증가해야 한다. 종전에는 종결 결과(success/error/skip)에서만
// 증가했고, 에러 반환 경로는 retry로 분류되어 wait에 섞이지 않아야 한다.
func TestReconcileTotal_CountsEveryReconcileOnce(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		setup    func(t *testing.T, ls *injectorv1alpha1.LoadScenario, r *LoadScenarioReconciler)
		conflict bool
		want     string
	}{
		{"run 시작", nil, false, "started"},
		{"실행 시각 대기", func(_ *testing.T, ls *injectorv1alpha1.LoadScenario, _ *LoadScenarioReconciler) {
			ls.CreationTimestamp.Time = time.Now()
		}, false, "wait"},
		{"run 정상 종료", func(_ *testing.T, ls *injectorv1alpha1.LoadScenario, _ *LoadScenarioReconciler) {
			start := metav1.NewTime(time.Now().Add(-2 * time.Minute))
			ls.Status.RunState = injectorv1alpha1.RunStateRunning
			ls.Status.RunStartTime = &start
		}, false, "success"},
		{"run 시작 실패", func(t *testing.T, _ *injectorv1alpha1.LoadScenario, r *LoadScenarioReconciler) {
			if err := r.K8sClient.CoreV1().Pods("ebpf-project").Delete(ctx, "suspect-pod", metav1.DeleteOptions{}); err != nil {
				t.Fatalf("delete target pod: %v", err)
			}
		}, false, "error"},
		{"lease 충돌 skip", func(t *testing.T, ls *injectorv1alpha1.LoadScenario, r *LoadScenarioReconciler) {
			ls.Spec.ConcurrencyPolicy = injectorv1alpha1.ConcurrencyForbid
			if _, err := safety.AcquireLock(ctx, r.K8sClient, r.LockNamespace, ls.Spec.TargetRef.Namespace, ls.Spec.TargetRef.Name, "other", time.Hour); err != nil {
				t.Fatalf("pre-acquire lease: %v", err)
			}
		}, false, "skip"},
		{"spec.suspend 정지", func(_ *testing.T, ls *injectorv1alpha1.LoadScenario, _ *LoadScenarioReconciler) {
			ls.Spec.Suspend = true
		}, false, "suspended"},
		{"maxFailures 초과 정지", func(_ *testing.T, ls *injectorv1alpha1.LoadScenario, _ *LoadScenarioReconciler) {
			ls.Spec.MaxFailures = 3
			ls.Status.ConsecutiveFailures = 3
		}, false, "suspended"},
		{"target namespace 거부", func(_ *testing.T, ls *injectorv1alpha1.LoadScenario, _ *LoadScenarioReconciler) {
			ls.Spec.TargetRef.Namespace = "kube-system"
		}, false, "denied"},
		{"cluster label 게이트 거부", func(_ *testing.T, _ *injectorv1alpha1.LoadScenario, r *LoadScenarioReconciler) {
			r.AllowClusterLabel = "environment=prod"
		}, false, "denied"},
		{"Running 전이 status update 충돌", nil, true, "retry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ls := scoreTriggerScenario()
			ls.Spec.ScoreTrigger = nil
			r, _ := newScoreReconciler(ls, nil, time.Now())
			if tc.setup != nil {
				tc.setup(t, ls, r)
			}
			// setup이 바꾼 spec과 status를 반영하도록 CR client를 다시 만든다.
			rebuildWithScenario(t, r, ls)
			if tc.conflict {
				r.Client = conflictOnStatusUpdate(ls)
			}
			before := map[string]float64{}
			for _, res := range reconcileResults {
				before[res] = reconcileTotal(res)
			}

			_, _ = r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}})

			for _, res := range reconcileResults {
				want := before[res]
				if res == tc.want {
					want++
				}
				if got := reconcileTotal(res); got != want {
					t.Errorf("result=%s 증가=%v, want %v", res, got-before[res], want-before[res])
				}
			}
		})
	}
}

// rebuildWithScenario는 setup이 바꾼 LoadScenario로 CR client를 다시 만들되 reconciler의 나머지
// 설정(K8sClient, 게이트 등)은 유지한다.
func rebuildWithScenario(t *testing.T, r *LoadScenarioReconciler, ls *injectorv1alpha1.LoadScenario) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = injectorv1alpha1.AddToScheme(scheme)
	r.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ls).WithStatusSubresource(ls).Build()
}

// TestReconcileTotal_DeletionCountedOnce는 삭제 정리 reconcile이 deleted로 1회 계수되는지 검증한다.
func TestReconcileTotal_DeletionCountedOnce(t *testing.T) {
	ctx := context.Background()
	ls := scoreTriggerScenario()
	ls.Spec.ScoreTrigger = nil
	r, c := newScoreReconciler(ls, nil, time.Now())
	if err := c.Delete(ctx, getScenario(t, c, ls)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	before := map[string]float64{}
	for _, res := range reconcileResults {
		before[res] = reconcileTotal(res)
	}

	reconcileOnce(t, r, ls)

	for _, res := range reconcileResults {
		want := before[res]
		if res == "deleted" {
			want++
		}
		if got := reconcileTotal(res); got != want {
			t.Errorf("result=%s 증가=%v, want %v", res, got-before[res], want-before[res])
		}
	}
}
