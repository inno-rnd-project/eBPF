package controller

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	injectorv1alpha1 "netobs/api/v1alpha1"
)

// reconcileTotal 은 result 라벨별 reconcile_total 현재값이다.
func reconcileTotal(result string) float64 {
	return testutil.ToFloat64(ReconcileTotal.WithLabelValues(result))
}

// TestReconcileTotal_CountsEveryReconcileOnce 는 #462 의 계수 정합을 검증한다. 종전에는 종결
// 결과(success/error/skip)에서만 증가해 대기, 거부, 정지, run 시작 reconcile 이 빠졌다. 각 경로가
// 해당 result 로 정확히 1회 계수되는지 단정한다.
func TestReconcileTotal_CountsEveryReconcileOnce(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(ls *injectorv1alpha1.LoadScenario)
		want   string
	}{
		{"run 시작", func(ls *injectorv1alpha1.LoadScenario) {}, "started"},
		{"실행 시각 대기", func(ls *injectorv1alpha1.LoadScenario) {
			ls.CreationTimestamp.Time = time.Now()
		}, "wait"},
		{"spec.suspend 정지", func(ls *injectorv1alpha1.LoadScenario) { ls.Spec.Suspend = true }, "suspended"},
		{"target namespace 거부", func(ls *injectorv1alpha1.LoadScenario) {
			ls.Spec.TargetRef.Namespace = "kube-system"
		}, "denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ls := scoreTriggerScenario()
			ls.Spec.ScoreTrigger = nil
			tc.mutate(ls)
			r, _ := newScoreReconciler(ls, nil, time.Now())
			before := map[string]float64{}
			for _, res := range []string{"started", "wait", "suspended", "denied", "success", "error", "skip"} {
				before[res] = reconcileTotal(res)
			}

			reconcileOnce(t, r, ls)

			for res, b := range before {
				want := b
				if res == tc.want {
					want = b + 1
				}
				if got := reconcileTotal(res); got != want {
					t.Errorf("result=%s 증가=%v, want %v", res, got-b, want-b)
				}
			}
		})
	}
}
