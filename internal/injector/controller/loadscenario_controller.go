// Package controller 는 #102 의 LoadScenario reconciler 구현을 담는다. dev cluster 에서 schedule
// 따라 자동 부하 인가를 트리거하며 CLI mode 와 동일한 safety gate 4 종 (CheckDuration /
// CheckIntensity / CheckClusterLabel / AcquireLock) 과 loadgen 패키지를 재사용 한다.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	injectorv1alpha1 "netobs/api/v1alpha1"
	"netobs/internal/injector/loadgen"
	"netobs/internal/injector/safety"
)

// FinalizerName 은 LoadScenario 삭제 시 controller 가 cleanup 책임을 표시하기 위한 finalizer 다.
// lease 해제와 stress Pod 가비지 수거가 보장된 뒤에만 finalizer 가 제거된다.
const FinalizerName = "loadscenario.injector.netobs.io/finalizer"

// SpikeAlertAsserter 는 c5 commit 에서 채워질 spike alert 자동 검증 흐름 의 의존성 추상화 이다.
// c4 단계 에서는 nil 인터페이스 로 두어 spec.spikeAlertAssertion 이 true 이더라도 status 갱신을
// skip 한다. c5 가 prometheus query 구현체 를 주입 해 활성화 된다.
type SpikeAlertAsserter interface {
	Observe(ctx context.Context, sinceRunEnd time.Time) ([]string, error)
}

// LoadScenarioReconciler 는 LoadScenario CR 의 schedule 따라 reconcile 한다.
type LoadScenarioReconciler struct {
	client.Client
	K8sClient         kubernetes.Interface
	Scheme            *runtimeSchemeStub
	AllowClusterLabel string
	LockNamespace     string
	LockHolder        string
	SpikeAsserter     SpikeAlertAsserter
	CronParser        cron.Parser
	Now               func() time.Time
}

// runtimeSchemeStub 은 controller-runtime 의 runtime.Scheme alias 다. controller-runtime 의
// import path 를 본 패키지 호출 측 에 노출 하지 않기 위해 local alias 로 둔다.
type runtimeSchemeStub = struct{ _ int }

// defaultCronParser 는 standard cron 5 필드 (minute / hour / dom / month / dow) 와 descriptor
// (@every, @daily 등) 를 지원한다.
var defaultCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// SetupWithManager 는 controller-runtime manager 에 LoadScenario watch 와 reconciler 를 등록한다.
func (r *LoadScenarioReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = func() time.Time { return time.Now().UTC() }
	}
	r.CronParser = defaultCronParser
	r.Client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&injectorv1alpha1.LoadScenario{}).
		Complete(r)
}

// Reconcile 은 LoadScenario 1 개에 대한 단일 reconcile 루프다.
func (r *LoadScenarioReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("loadscenario", req.NamespacedName)
	defer func() { ReconcileTimestamp.Set(float64(time.Now().Unix())) }()

	var ls injectorv1alpha1.LoadScenario
	if err := r.Get(ctx, req.NamespacedName, &ls); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion timestamp 검사 - finalizer cleanup.
	if !ls.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &ls)
	}

	// finalizer 등록 (idempotent).
	if controllerutil.AddFinalizer(&ls, FinalizerName) {
		if err := r.Update(ctx, &ls); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// prod cluster 차단 gate. CheckClusterLabel 가 매칭 Node 가 0 개 이면 fail-fast.
	if err := safety.CheckClusterLabel(ctx, r.K8sClient, r.AllowClusterLabel); err != nil {
		logger.Info("cluster label gate refused", "error", err.Error())
		setCondition(&ls, "Ready", metav1.ConditionFalse, "ClusterLabelGate", err.Error())
		_ = r.Status().Update(ctx, &ls)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// spec.suspend 가 true 이면 skip.
	if ls.Spec.Suspend {
		setCondition(&ls, "Suspended", metav1.ConditionTrue, "SpecSuspend", "spec.suspend=true")
		_ = r.Status().Update(ctx, &ls)
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// schedule parse + next run time 산정.
	schedule, err := r.CronParser.Parse(ls.Spec.Schedule)
	if err != nil {
		setCondition(&ls, "Ready", metav1.ConditionFalse, "InvalidSchedule", err.Error())
		_ = r.Status().Update(ctx, &ls)
		return ctrl.Result{}, nil
	}
	now := r.Now()
	var lastSchedule time.Time
	if ls.Status.LastScheduleTime != nil {
		lastSchedule = ls.Status.LastScheduleTime.Time
	} else {
		lastSchedule = ls.CreationTimestamp.Time
	}
	nextRun := schedule.Next(lastSchedule)
	if nextRun.After(now) {
		return ctrl.Result{RequeueAfter: nextRun.Sub(now)}, nil
	}

	// 동시 injection 정책 처리 + safety gate + lock acquire + load 실행.
	ActiveCount.Inc()
	runErr := r.runScenario(ctx, &ls, now)
	ActiveCount.Dec()
	if runErr != nil {
		logger.Error(runErr, "run scenario failed")
		ls.Status.ConsecutiveFailures++
		if ls.Spec.MaxFailures > 0 && ls.Status.ConsecutiveFailures >= ls.Spec.MaxFailures {
			ls.Spec.Suspend = true
			_ = r.Update(ctx, &ls)
			setCondition(&ls, "Suspended", metav1.ConditionTrue, "MaxFailuresExceeded",
				fmt.Sprintf("consecutiveFailures=%d >= maxFailures=%d", ls.Status.ConsecutiveFailures, ls.Spec.MaxFailures))
		}
		setCondition(&ls, "Scheduled", metav1.ConditionFalse, "RunFailed", runErr.Error())
		_ = r.Status().Update(ctx, &ls)
		RecordReconcileResult(ls.Namespace, ls.Name, "error", float64(time.Now().Unix()))
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// 정상 종료.
	ls.Status.ConsecutiveFailures = 0
	successTime := metav1.NewTime(r.Now())
	ls.Status.LastSuccessfulRunTime = &successTime
	setCondition(&ls, "Ready", metav1.ConditionTrue, "ReconcileOK", "controller is reconciling scenario")
	setCondition(&ls, "Scheduled", metav1.ConditionTrue, "RunSucceeded", "run completed within duration")
	if err := r.Status().Update(ctx, &ls); err != nil {
		return ctrl.Result{}, err
	}
	RecordReconcileResult(ls.Namespace, ls.Name, "success", float64(time.Now().Unix()))
	nextAfter := schedule.Next(r.Now()).Sub(r.Now())
	if nextAfter <= 0 {
		nextAfter = time.Minute
	}
	return ctrl.Result{RequeueAfter: nextAfter}, nil
}

// runScenario 는 safety gate 와 lock acquire 와 load 실행 의 blocking 흐름이다. concurrencyPolicy
// 에 따라 lock 충돌 처리 가 분기 한다.
func (r *LoadScenarioReconciler) runScenario(ctx context.Context, ls *injectorv1alpha1.LoadScenario, startTime time.Time) error {
	kind := loadgen.Kind(ls.Spec.Kind)
	if err := safety.CheckDuration(ls.Spec.Duration.Duration); err != nil {
		return fmt.Errorf("safety CheckDuration: %w", err)
	}
	if err := safety.CheckIntensity(kind, ls.Spec.Intensity); err != nil {
		return fmt.Errorf("safety CheckIntensity: %w", err)
	}

	// concurrencyPolicy = Replace 의 경우 진행 중 lease 를 강제 해제 한 뒤 시도.
	if ls.Spec.ConcurrencyPolicy == injectorv1alpha1.ConcurrencyReplace {
		if err := r.forceReleaseLease(ctx, ls); err != nil {
			return fmt.Errorf("replace lease: %w", err)
		}
	}

	release, err := safety.AcquireLock(ctx, r.K8sClient, r.LockNamespace,
		ls.Spec.TargetRef.Namespace, ls.Spec.TargetRef.Name, r.LockHolder,
		ls.Spec.Duration.Duration*2)
	if err != nil {
		// concurrencyPolicy = Forbid 의 경우 lock 충돌 은 정상 skip 으로 분류.
		if ls.Spec.ConcurrencyPolicy == injectorv1alpha1.ConcurrencyForbid {
			scheduleTime := metav1.NewTime(startTime)
			ls.Status.LastScheduleTime = &scheduleTime
			setCondition(ls, "Scheduled", metav1.ConditionFalse, "ForbidLockHeld", err.Error())
			return nil
		}
		return fmt.Errorf("acquire lock: %w", err)
	}
	defer release()

	scheduleTime := metav1.NewTime(startTime)
	ls.Status.LastScheduleTime = &scheduleTime

	// target Pod fetch 후 nodeName 추출. cpu / memory / gpu loadgen 이 stress Pod 를 동일 node 에
	// 강제 배치 하기 위해 Params.TargetNode 가 비어 있으면 안 된다. CLI mode 의 verifyTargetPod 와
	// 동일 단계 다.
	targetPod, err := r.K8sClient.CoreV1().Pods(ls.Spec.TargetRef.Namespace).Get(ctx, ls.Spec.TargetRef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get target pod %s/%s: %w", ls.Spec.TargetRef.Namespace, ls.Spec.TargetRef.Name, err)
	}
	if targetPod.Spec.NodeName == "" {
		return fmt.Errorf("target pod %s/%s has no nodeName (Pod not yet scheduled?)", ls.Spec.TargetRef.Namespace, ls.Spec.TargetRef.Name)
	}

	gen, err := loadgen.New(kind, r.K8sClient)
	if err != nil {
		return fmt.Errorf("loadgen New: %w", err)
	}
	params := loadgen.Params{
		TargetNamespace: ls.Spec.TargetRef.Namespace,
		TargetPod:       ls.Spec.TargetRef.Name,
		TargetNode:      targetPod.Spec.NodeName,
		SpawnNamespace:  ls.Namespace,
		Duration:        ls.Spec.Duration.Duration,
		Intensity:       ls.Spec.Intensity,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "loadscenario-controller",
			"loadscenario.name":            ls.Name,
		},
	}
	if err := gen.Start(ctx, params); err != nil {
		return fmt.Errorf("loadgen Start: %w", err)
	}
	defer func() { _ = gen.Stop(context.Background()) }()

	// blocking duration wait. controller worker stall 우려 가 있어 운영 가이드 에 max 30 분 명시.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(ls.Spec.Duration.Duration):
	}

	// spike alert 자동 검증 흐름은 c5 commit 에서 SpikeAsserter 가 주입 되면 활성화.
	if ls.Spec.SpikeAlertAssertion && r.SpikeAsserter != nil {
		alerts, observeErr := r.SpikeAsserter.Observe(ctx, startTime.Add(ls.Spec.Duration.Duration))
		if observeErr != nil {
			setCondition(ls, "SpikeAlertObserved", metav1.ConditionUnknown, "SpikeAssertError", observeErr.Error())
		} else {
			ls.Status.LastObservedSpikeAlerts = alerts
			status := metav1.ConditionFalse
			reason := "NoSpikeAlertFiring"
			msg := "no spike alert observed in window"
			if len(alerts) > 0 {
				status = metav1.ConditionTrue
				reason = "SpikeAlertFiring"
				msg = fmt.Sprintf("observed alerts=%v", alerts)
			}
			setCondition(ls, "SpikeAlertObserved", status, reason, msg)
		}
	}

	return nil
}

// handleDeletion 은 DeletionTimestamp 가 설정된 LoadScenario 의 cleanup 흐름이다. lease 해제 와
// stress Pod 가비지 수거 후 finalizer 를 제거 한다.
func (r *LoadScenarioReconciler) handleDeletion(ctx context.Context, ls *injectorv1alpha1.LoadScenario) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ls, FinalizerName) {
		return ctrl.Result{}, nil
	}
	if err := r.forceReleaseLease(ctx, ls); err != nil {
		// lease 가 없으면 정상으로 간주.
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	// stress Pod 가비지 수거 - loadscenario.name 라벨 매칭 Pod 삭제.
	if err := r.cleanupStressPods(ctx, ls); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(ls, FinalizerName)
	if err := r.Update(ctx, ls); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *LoadScenarioReconciler) forceReleaseLease(ctx context.Context, ls *injectorv1alpha1.LoadScenario) error {
	name := safety.LockName(ls.Spec.TargetRef.Namespace, ls.Spec.TargetRef.Name)
	return r.K8sClient.CoreV1().ConfigMaps(r.LockNamespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (r *LoadScenarioReconciler) cleanupStressPods(ctx context.Context, ls *injectorv1alpha1.LoadScenario) error {
	pods, err := r.K8sClient.CoreV1().Pods(ls.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("loadscenario.name=%s", ls.Name),
	})
	if err != nil {
		return err
	}
	for _, p := range pods.Items {
		_ = r.K8sClient.CoreV1().Pods(ls.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{})
	}
	return nil
}

// setCondition 은 metav1.Condition 표준 helper 다. 같은 type 의 condition 이 이미 있으면 갱신,
// 없으면 추가.
func setCondition(ls *injectorv1alpha1.LoadScenario, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&ls.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
}

// 빈 import guards - 본 파일이 corev1 / types 를 직접 import 하지 않더라도 future commit (c5/c6)
// 에서 자주 추가 되므로 lint warning 회피 용 placeholder 가 필요 한 경우 본 위치 에 사용.
var _ = corev1.PodSpec{}
var _ types.NamespacedName
