package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	injectorv1alpha1 "netobs/api/v1alpha1"
	"netobs/internal/injector/safety"
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
		AllowedNamespaces: []string{"ebpf-project", "default"},
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
	if provider.calls != 0 {
		t.Errorf("provider 호출=%d want 0 (debounce 는 MaxScore 호출 전에 차단)", provider.calls)
	}
}

// TestReconcile_ScoreTrigger_NoDebounceOnFailedStart 는 score 가 임계 를 넘어 트리거 했더라도 startRun
// 이 실패 하면 (target pod 부재) LastScoreTriggerTime 이 기록 되지 않아 다음 poll 에서 debounce 없이
// 재시도 가능 한지 검증 한다.
func TestReconcile_ScoreTrigger_NoDebounceOnFailedStart(t *testing.T) {
	ls := scoreTriggerScenario()
	provider := &fakeScoreProvider{score: 0.9, found: true}
	r, c := newScoreReconciler(ls, provider, time.Now())
	// target pod 를 제거 해 startRun 의 target fetch 가 실패 하게 한다.
	if err := r.K8sClient.CoreV1().Pods("ebpf-project").Delete(context.Background(), "suspect-pod", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete target pod: %v", err)
	}

	reconcileOnce(t, r, ls)

	got := getScenario(t, c, ls)
	if got.Status.RunState == injectorv1alpha1.RunStateRunning {
		t.Error("runState=Running, want not-Running (startRun 실패)")
	}
	if got.Status.LastScoreTriggerTime != nil {
		t.Error("lastScoreTriggerTime 기록됨, want nil (run 미시작 시 debounce 미적용)")
	}
	if got.Status.ConsecutiveFailures != 1 {
		t.Errorf("consecutiveFailures=%d want 1 (startRun 실패 카운트)", got.Status.ConsecutiveFailures)
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

// TestReconcile_NamespaceGate 는 #420 의 허용 namespace 게이트를 단정한다. 목록 밖 target 은
// NamespaceDenied condition 과 함께 거부되어 stress pod 이 생기지 않고, fail-closed (빈 목록
// 전부 거부) 도 함께 고정한다.
func TestReconcile_NamespaceGate(t *testing.T) {
	ls := scoreTriggerScenario()
	ls.Spec.ScoreTrigger = nil
	ls.Spec.TargetRef.Namespace = "kube-system"
	r, crClient := newScoreReconciler(ls, &fakeScoreProvider{score: 1, found: true}, time.Now())

	reconcileOnce(t, r, ls)

	got := getScenario(t, crClient, ls)
	var denied bool
	for _, c := range got.Status.Conditions {
		if c.Type == "Ready" && c.Reason == "NamespaceDenied" {
			denied = true
		}
	}
	if !denied {
		t.Errorf("NamespaceDenied condition 미설정: %+v", got.Status.Conditions)
	}
	pods, _ := r.K8sClient.CoreV1().Pods("ebpf-project").List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=stress"})
	if len(pods.Items) != 0 {
		t.Errorf("거부된 run 이 stress pod %d개 생성", len(pods.Items))
	}

	// fail-closed: 빈 허용 목록은 spawn namespace 부터 거부한다 (safety 단위 검증).
	if err := safety.CheckNamespaceAllowed(nil, "ebpf-project"); err == nil {
		t.Error("빈 허용 목록이 거부되지 않음 (fail-closed 위반)")
	}
}

// TestReconcile_StartFailureWaitsForNextScheduleTick 는 #458 의 재시도 간격 회귀 가드다. run 시작 전
// 실패 직후의 reconcile (실패 status 갱신이 일으키는 이벤트) 이 곧바로 재시도하지 않고 다음 schedule
// tick 까지 기다려야 한다. 종전에는 초 단위로 재시도해 기본 maxFailures 3 을 바로 소진했다. cron 경로와
// score 트리거 경로 모두에 적용되며, score 경로는 대기 중 score 조회도 하지 않아야 한다.
func TestReconcile_StartFailureWaitsForNextScheduleTick(t *testing.T) {
	cases := []struct {
		name     string
		scenario func() *injectorv1alpha1.LoadScenario
		provider *fakeScoreProvider
	}{
		{"cron 경로", func() *injectorv1alpha1.LoadScenario {
			ls := scoreTriggerScenario()
			ls.Spec.ScoreTrigger = nil
			return ls
		}, nil},
		{"score 트리거 경로", scoreTriggerScenario, &fakeScoreProvider{score: 0.9, found: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ls := tc.scenario()
			var provider InterferenceScoreProvider
			if tc.provider != nil {
				provider = tc.provider
			}
			clock := time.Now()
			r, c := newScoreReconciler(ls, provider, clock)
			r.Now = func() time.Time { return clock }
			createCalls := 0
			r.K8sClient.(*k8sfake.Clientset).PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				createCalls++
				return true, nil, errors.New("injected create failure")
			})

			reconcileOnce(t, r, ls)
			got := getScenario(t, c, ls)
			if got.Status.ConsecutiveFailures != 1 || got.Status.LastScheduleTime == nil {
				t.Fatalf("첫 실패 failures=%d lastScheduleTime=%v, want 1 / 기록됨", got.Status.ConsecutiveFailures, got.Status.LastScheduleTime)
			}
			scoreCalls := 0
			if tc.provider != nil {
				scoreCalls = tc.provider.calls
			}

			// 같은 시각의 연속 reconcile 은 재시도하지 않는다.
			for i := 0; i < 3; i++ {
				reconcileOnce(t, r, getScenario(t, c, ls))
			}
			got = getScenario(t, c, ls)
			if got.Status.ConsecutiveFailures != 1 || createCalls != 1 {
				t.Errorf("tick 전 재시도 failures=%d createCalls=%d, want 1 / 1", got.Status.ConsecutiveFailures, createCalls)
			}
			if tc.provider != nil && tc.provider.calls != scoreCalls {
				t.Errorf("tick 전 score 조회 %d회 추가, want 0", tc.provider.calls-scoreCalls)
			}

			// 다음 tick (@every 1m) 이 지나면 재시도한다.
			clock = clock.Add(61 * time.Second)
			reconcileOnce(t, r, getScenario(t, c, ls))
			got = getScenario(t, c, ls)
			if got.Status.ConsecutiveFailures != 2 || createCalls != 2 {
				t.Errorf("tick 후 재시도 failures=%d createCalls=%d, want 2 / 2", got.Status.ConsecutiveFailures, createCalls)
			}
		})
	}
}

// leaseExists 는 startRun 이 잡는 target 별 lease ConfigMap 의 존재 여부를 돌려준다.
func leaseExists(t *testing.T, r *LoadScenarioReconciler, ls *injectorv1alpha1.LoadScenario) bool {
	t.Helper()
	name := safety.LockName(ls.Spec.TargetRef.Namespace, ls.Spec.TargetRef.Name)
	_, err := r.K8sClient.CoreV1().ConfigMaps(r.LockNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get lease: %v", err)
	}
	return false
}

// TestStartRun_ReleasesLeaseOnPreStartFailure 는 #458 의 회귀 가드다. lease 획득 후 run 이 시작되기
// 전에 실패하는 경로 (target Pod 조회 실패, nodeName 빈 값, loadgen Start 실패) 에서 lease 가 남지
// 않아야 한다. 종전에는 Start 실패만 해제해 나머지 경로가 lease 를 TTL (최대 60분) 동안 남겼고, 기본
// Forbid 정책에서 그 target 의 후속 시나리오가 전부 skip 됐다.
func TestStartRun_ReleasesLeaseOnPreStartFailure(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, r *LoadScenarioReconciler)
	}{
		{"target pod 조회 실패", func(t *testing.T, r *LoadScenarioReconciler) {
			if err := r.K8sClient.CoreV1().Pods("ebpf-project").Delete(context.Background(), "suspect-pod", metav1.DeleteOptions{}); err != nil {
				t.Fatalf("delete target pod: %v", err)
			}
		}},
		{"nodeName 빈 값", func(t *testing.T, r *LoadScenarioReconciler) {
			p, err := r.K8sClient.CoreV1().Pods("ebpf-project").Get(context.Background(), "suspect-pod", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("get target pod: %v", err)
			}
			p.Spec.NodeName = ""
			if _, err := r.K8sClient.CoreV1().Pods("ebpf-project").Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
				t.Fatalf("update target pod: %v", err)
			}
		}},
		{"loadgen Start 실패", func(t *testing.T, r *LoadScenarioReconciler) {
			r.K8sClient.(*k8sfake.Clientset).PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("injected create failure")
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ls := scoreTriggerScenario()
			r, _ := newScoreReconciler(ls, nil, time.Now())
			tc.setup(t, r)

			if err := r.startRun(context.Background(), ls, time.Now()); err == nil {
				t.Fatalf("startRun err=nil, want 실패")
			}
			if leaseExists(t, r, ls) {
				t.Errorf("lease 잔존, want 시작 전 실패 시 해제")
			}
		})
	}
}

// TestStartRun_KeepsLeaseOnSuccess 는 run 이 시작된 경우 lease 가 유지되는지 검증한다. 성공 경로의
// lease 는 Running phase 종료 시점 의 forceReleaseLease 나 TTL 로 정리 되어야 한다.
func TestStartRun_KeepsLeaseOnSuccess(t *testing.T) {
	ls := scoreTriggerScenario()
	r, _ := newScoreReconciler(ls, nil, time.Now())

	if err := r.startRun(context.Background(), ls, time.Now()); err != nil {
		t.Fatalf("startRun err=%v", err)
	}
	if !leaseExists(t, r, ls) {
		t.Errorf("lease 미보유, want run 시작 후 유지")
	}
}

// TestReconcile_RetriesTargetAfterPreStartFailure 는 시작 전 실패 뒤의 후속 reconcile 이 lease 에 막히지
// 않고 다음 schedule tick 에 같은 target 을 다시 시작하는지 검증한다. 기본 Forbid 정책에서 lease 가
// 남으면 이 재시도가 TTL (최대 60분) 동안 ForbidLockHeld skip 으로 막혔다.
func TestReconcile_RetriesTargetAfterPreStartFailure(t *testing.T) {
	ls := scoreTriggerScenario()
	ls.Spec.ScoreTrigger = nil
	ls.Spec.ConcurrencyPolicy = injectorv1alpha1.ConcurrencyForbid
	clock := time.Now()
	r, c := newScoreReconciler(ls, nil, clock)
	r.Now = func() time.Time { return clock }
	ctx := context.Background()

	// 대상 Pod 가 아직 노드에 배치되지 않은 상태로 첫 reconcile 이 실패한다.
	p, err := r.K8sClient.CoreV1().Pods("ebpf-project").Get(ctx, "suspect-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get target pod: %v", err)
	}
	p.Spec.NodeName = ""
	if _, err := r.K8sClient.CoreV1().Pods("ebpf-project").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update target pod: %v", err)
	}
	reconcileOnce(t, r, ls)
	if _, reason := conditionReason(getScenario(t, c, ls), "Scheduled"); reason != "RunStartFailed" {
		t.Fatalf("첫 reconcile Scheduled reason=%q, want RunStartFailed", reason)
	}
	if leaseExists(t, r, ls) {
		t.Fatalf("시작 전 실패 후 lease 잔존")
	}

	// 대상 Pod 가 배치되고 다음 tick 이 지나면 lease 충돌 없이 run 을 시작해야 한다.
	p.Spec.NodeName = "node1"
	if _, err := r.K8sClient.CoreV1().Pods("ebpf-project").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update target pod: %v", err)
	}
	clock = clock.Add(61 * time.Second)
	reconcileOnce(t, r, getScenario(t, c, ls))

	got := getScenario(t, c, ls)
	if _, reason := conditionReason(got, "Scheduled"); reason == "ForbidLockHeld" {
		t.Fatalf("후속 reconcile 이 ForbidLockHeld 로 skip, want 재시작")
	}
	if got.Status.RunState != injectorv1alpha1.RunStateRunning {
		t.Errorf("runState=%q, want Running", got.Status.RunState)
	}
}
