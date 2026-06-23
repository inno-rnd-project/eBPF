package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	injectorv1alpha1 "netobs/api/v1alpha1"
)

// fakeScoreProvider 는 InterferenceScoreProvider 의 테스트 더블 이다. 호출 횟수 를 기록 해 safety gate
// 가 score 평가 보다 먼저 차단 하는지 검증 에 쓴다.
type fakeScoreProvider struct {
	score float64
	found bool
	err   error
	calls int
}

func (f *fakeScoreProvider) MaxScore(_ context.Context, _, _ PodRef, _ string) (float64, bool, error) {
	f.calls++
	return f.score, f.found, f.err
}

func scoreTriggerScenario() *injectorv1alpha1.LoadScenario {
	return &injectorv1alpha1.LoadScenario{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "score-ls",
			Namespace:         "ebpf-project",
			Finalizers:        []string{FinalizerName},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: injectorv1alpha1.LoadScenarioSpec{
			Schedule:  "@every 1m",
			Kind:      injectorv1alpha1.LoadKindCPU,
			Duration:  metav1.Duration{Duration: time.Minute},
			Intensity: "500m",
			TargetRef: injectorv1alpha1.LoadScenarioTargetRef{Namespace: "ebpf-project", Name: "suspect-pod"},
			ScoreTrigger: &injectorv1alpha1.ScoreTriggerSpec{
				VictimRef:      injectorv1alpha1.LoadScenarioTargetRef{Namespace: "default", Name: "victim-pod"},
				Dimension:      "cpu",
				ScoreThreshold: "0.7",
				MinInterval:    metav1.Duration{Duration: 10 * time.Minute},
			},
		},
	}
}

func newScoreReconciler(ls *injectorv1alpha1.LoadScenario, provider InterferenceScoreProvider, now time.Time) (*LoadScenarioReconciler, client.Client) {
	scheme := runtime.NewScheme()
	_ = injectorv1alpha1.AddToScheme(scheme)
	crClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ls).
		WithStatusSubresource(ls).
		Build()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"environment": "dev"}},
	}
	targetPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "suspect-pod", Namespace: "ebpf-project"},
		Spec:       corev1.PodSpec{NodeName: "node1"},
	}
	k8s := k8sfake.NewSimpleClientset(node, targetPod)

	r := &LoadScenarioReconciler{
		Client:            crClient,
		K8sClient:         k8s,
		AllowClusterLabel: "environment=dev",
		LockNamespace:     "ebpf-project",
		LockHolder:        "test",
		ScoreProvider:     provider,
		CronParser:        defaultCronParser,
		Now:               func() time.Time { return now },
	}
	return r, crClient
}

func reconcileOnce(t *testing.T, r *LoadScenarioReconciler, ls *injectorv1alpha1.LoadScenario) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

func getScenario(t *testing.T, c client.Client, ls *injectorv1alpha1.LoadScenario) *injectorv1alpha1.LoadScenario {
	t.Helper()
	var got injectorv1alpha1.LoadScenario
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ls.Namespace, Name: ls.Name}, &got); err != nil {
		t.Fatalf("get scenario: %v", err)
	}
	return &got
}

func conditionReason(ls *injectorv1alpha1.LoadScenario, condType string) (metav1.ConditionStatus, string) {
	for _, c := range ls.Status.Conditions {
		if c.Type == condType {
			return c.Status, c.Reason
		}
	}
	return "", ""
}

// TestReconcile_ScoreTrigger_AboveThreshold 는 score 가 임계 이상 이면 run 이 트리거 (RunState=Running)
// 되고 stress Pod 가 spawn 되며 ScoreTriggered=True 가 기록 되는지 검증 한다.
func TestReconcile_ScoreTrigger_AboveThreshold(t *testing.T) {
	ls := scoreTriggerScenario()
	provider := &fakeScoreProvider{score: 0.85, found: true}
	r, c := newScoreReconciler(ls, provider, time.Now())

	reconcileOnce(t, r, ls)

	got := getScenario(t, c, ls)
	if got.Status.RunState != injectorv1alpha1.RunStateRunning {
		t.Errorf("runState=%q want Running (score 0.85 >= 0.7)", got.Status.RunState)
	}
	if st, reason := conditionReason(got, "ScoreTriggered"); st != metav1.ConditionTrue || reason != "ThresholdMet" {
		t.Errorf("ScoreTriggered=%s/%s want True/ThresholdMet", st, reason)
	}
	if got.Status.LastScoreTriggerTime == nil {
		t.Error("lastScoreTriggerTime nil, want set")
	}
	pods, _ := r.K8sClient.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{LabelSelector: "loadscenario.name=score-ls"})
	if len(pods.Items) != 1 {
		t.Errorf("stress pod 수=%d want 1", len(pods.Items))
	}
}

// TestReconcile_ScoreTrigger_BelowThreshold 는 score 가 임계 미만 이면 run 을 트리거 하지 않고
// ScoreTriggered=False/BelowThreshold 로 Idle 을 유지 하는지 검증 한다.
func TestReconcile_ScoreTrigger_BelowThreshold(t *testing.T) {
	ls := scoreTriggerScenario()
	provider := &fakeScoreProvider{score: 0.5, found: true}
	r, c := newScoreReconciler(ls, provider, time.Now())

	reconcileOnce(t, r, ls)

	got := getScenario(t, c, ls)
	if got.Status.RunState == injectorv1alpha1.RunStateRunning {
		t.Errorf("runState=Running, want Idle (score 0.5 < 0.7)")
	}
	if st, reason := conditionReason(got, "ScoreTriggered"); st != metav1.ConditionFalse || reason != "BelowThreshold" {
		t.Errorf("ScoreTriggered=%s/%s want False/BelowThreshold", st, reason)
	}
}

// TestReconcile_ScoreTrigger_Debounced 는 임계 를 넘어도 minInterval 이내 면 재트리거 하지 않고
// ScoreTriggered=False/Debounced 를 기록 하는지 검증 한다.
func TestReconcile_ScoreTrigger_Debounced(t *testing.T) {
	now := time.Now()
	ls := scoreTriggerScenario()
	recent := metav1.NewTime(now.Add(-time.Minute)) // minInterval(10m) 이내
	ls.Status.LastScoreTriggerTime = &recent
	provider := &fakeScoreProvider{score: 0.95, found: true}
	r, c := newScoreReconciler(ls, provider, now)

	reconcileOnce(t, r, ls)

	got := getScenario(t, c, ls)
	if got.Status.RunState == injectorv1alpha1.RunStateRunning {
		t.Error("runState=Running, want Idle (debounce)")
	}
	if st, reason := conditionReason(got, "ScoreTriggered"); st != metav1.ConditionFalse || reason != "Debounced" {
		t.Errorf("ScoreTriggered=%s/%s want False/Debounced", st, reason)
	}
}

// TestReconcile_ScoreTrigger_NilProvider 는 provider 미주입 시 트리거 하지 않고 NoScoreProvider 로
// degrade 하는지 검증 한다.
func TestReconcile_ScoreTrigger_NilProvider(t *testing.T) {
	ls := scoreTriggerScenario()
	r, c := newScoreReconciler(ls, nil, time.Now())

	reconcileOnce(t, r, ls)

	got := getScenario(t, c, ls)
	if got.Status.RunState == injectorv1alpha1.RunStateRunning {
		t.Error("runState=Running, want Idle (provider 부재)")
	}
	if st, reason := conditionReason(got, "ScoreTriggered"); st != metav1.ConditionUnknown || reason != "NoScoreProvider" {
		t.Errorf("ScoreTriggered=%s/%s want Unknown/NoScoreProvider", st, reason)
	}
}

// TestReconcile_ScoreTrigger_SuspendBlocksEval 은 suspend 가드 가 score 평가 보다 먼저 차단 해
// provider 가 호출 되지 않는지 검증 한다 (안전장치 가 자동 트리거 에도 우선 적용됨).
func TestReconcile_ScoreTrigger_SuspendBlocksEval(t *testing.T) {
	ls := scoreTriggerScenario()
	ls.Spec.Suspend = true
	provider := &fakeScoreProvider{score: 0.99, found: true}
	r, c := newScoreReconciler(ls, provider, time.Now())

	reconcileOnce(t, r, ls)

	if provider.calls != 0 {
		t.Errorf("provider 호출=%d want 0 (suspend 가 score 평가보다 먼저 차단)", provider.calls)
	}
	got := getScenario(t, c, ls)
	if got.Status.RunState == injectorv1alpha1.RunStateRunning {
		t.Error("runState=Running, want Idle (suspend)")
	}
	if st, _ := conditionReason(got, "Suspended"); st != metav1.ConditionTrue {
		t.Errorf("Suspended=%s want True", st)
	}
}
