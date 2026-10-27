package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	injectorv1alpha1 "netobs/api/v1alpha1"
)

// fakeSpikeAsserter는 고정된 alert 목록을 돌려주는 SpikeAlertAsserter 테스트 더블이다.
type fakeSpikeAsserter struct{ alerts []string }

func (f *fakeSpikeAsserter) Observe(context.Context, time.Time) ([]string, error) {
	return f.alerts, nil
}

// TestSpikeAssertion_StaleHitClearedOnNoFireRun은 #462의 기록 정합을 검증한다. 직전 run이 hit여서
// LastObservedSpikeAlerts가 채워진 상태에서 이번 run이 미발화로 끝나면, 필드가 비고
// SpikeAlertObserved=False가 되어야 한다. 기록 전용 계약대로 run은 성공으로 집계된다.
func TestSpikeAssertion_StaleHitClearedOnNoFireRun(t *testing.T) {
	clock := time.Now()
	ls := scoreTriggerScenario()
	ls.Spec.ScoreTrigger = nil
	ls.Spec.SpikeAlertAssertion = true
	runStart := metav1.NewTime(clock.Add(-2 * time.Minute))
	ls.Status.RunState = injectorv1alpha1.RunStateRunning
	ls.Status.RunStartTime = &runStart
	ls.Status.LastObservedSpikeAlerts = []string{"CPUThrottleSpikeDetected"}
	r, c := newScoreReconciler(ls, nil, clock)
	r.Now = func() time.Time { return clock }
	r.SpikeAsserter = &fakeSpikeAsserter{}

	// run 종료: AwaitingSpikeAlert 진입 시 직전 hit 목록이 비워진다.
	reconcileOnce(t, r, ls)
	got := getScenario(t, c, ls)
	if got.Status.RunState != injectorv1alpha1.RunStateAwaitingSpikeAlert {
		t.Fatalf("runState=%q, want AwaitingSpikeAlert", got.Status.RunState)
	}
	if len(got.Status.LastObservedSpikeAlerts) != 0 {
		t.Errorf("polling 시작 시 lastObservedSpikeAlerts=%v, want 비움", got.Status.LastObservedSpikeAlerts)
	}

	// polling window 만료: 미발화로 끝나도 run은 성공으로 집계된다.
	clock = clock.Add(SpikePollWindow + time.Minute)
	reconcileOnce(t, r, got)
	got = getScenario(t, c, ls)
	if status, reason := conditionReason(got, "SpikeAlertObserved"); status != metav1.ConditionFalse || reason != "NoSpikeAlertFiring" {
		t.Errorf("SpikeAlertObserved=%s/%s, want False/NoSpikeAlertFiring", status, reason)
	}
	if len(got.Status.LastObservedSpikeAlerts) != 0 {
		t.Errorf("미발화 run 뒤 lastObservedSpikeAlerts=%v, want 비움", got.Status.LastObservedSpikeAlerts)
	}
	if got.Status.LastSuccessfulRunTime == nil || got.Status.RunState != injectorv1alpha1.RunStateIdle {
		t.Errorf("run이 성공으로 집계되지 않음: lastSuccessfulRunTime=%v runState=%q", got.Status.LastSuccessfulRunTime, got.Status.RunState)
	}
}
