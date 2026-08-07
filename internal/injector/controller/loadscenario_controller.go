// Package controller 는 #102 의 LoadScenario reconciler 구현을 담는다. dev cluster 에서 schedule
// 따라 자동 부하 인가를 트리거하며 CLI mode 와 동일한 safety gate 4 종 (CheckDuration /
// CheckIntensity / CheckClusterLabel / AcquireLock) 과 loadgen 패키지를 재사용 한다.
//
// 비동기 state machine 설계.
//
// Reconcile 은 blocking wait 없이 짧게 끝나며 다음 transition 까지 RequeueAfter 로 wait. 본 설계 는
// controller worker 가 부하 인가 시간 (최대 30 분) 동안 점유 되어 다른 LoadScenario reconcile 이
// starvation 되는 것 과 deletion / suspend 요청 의 반응성 저하 를 차단 한다.
//
//	Idle  ── schedule due ──▶  Running  ── duration 경과 ──▶  AwaitingSpikeAlert (spec.spikeAlertAssertion=true)
//	  ▲                                                                │
//	  └──────────────  poll window 만료 (5 분) 또는 spike hit  ◀────────┘
//
//	Idle  ── schedule due, spec.spikeAlertAssertion=false ──▶  Running  ── duration 경과 ──▶  Idle (success)
package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// SpikePollWindow 는 AwaitingSpikeAlert phase 의 polling 만료 시간 이다. 본 시간 안에 spike alert
// hit 가 1 회 라도 발생 하면 SpikeAlertObserved=True 로 기록 되고 Idle 로 전환 한다.
const SpikePollWindow = 5 * time.Minute

// SpikePollInterval 은 AwaitingSpikeAlert phase 의 reconcile 재호출 간격 이다. 매 호출 시 단일
// query 가 수행 되어 reconcile worker 의 blocking 시간 이 짧게 유지 된다.
const SpikePollInterval = 30 * time.Second

// errSkipForbidLockHeld 는 concurrencyPolicy=Forbid 의 lock 충돌 시 success 가 아닌 skip 결과로
// 분류 하기 위한 sentinel error 다. Reconcile 본체 에서 errors.Is 로 검사 한다.
var errSkipForbidLockHeld = errors.New("forbid policy: lock held by another injection")

// SpikeAlertAsserter 는 spike alert 자동 검증 흐름 의 의존성 추상화 이다. PromSpikeAsserter 가
// 본 인터페이스 의 구현체 로 controller 에 주입 된다.
type SpikeAlertAsserter interface {
	Observe(ctx context.Context, sinceRunEnd time.Time) ([]string, error)
}

// LoadScenarioReconciler 는 LoadScenario CR 의 schedule 따라 reconcile 한다.
type LoadScenarioReconciler struct {
	client.Client
	K8sClient         kubernetes.Interface
	AllowClusterLabel string
	LockNamespace     string
	LockHolder        string
	SpikeAsserter     SpikeAlertAsserter
	// ScoreProvider 는 spec.scoreTrigger 평가 시 correlation 간섭 score 를 조회 한다. nil 이면
	// scoreTrigger 가 설정 돼도 트리거 하지 않고 condition 으로 degrade 를 표시 한다.
	ScoreProvider InterferenceScoreProvider
	CronParser    cron.Parser
	Now           func() time.Time
	// SelfPod* 는 downward API 로 주입된 컨트롤러 자기 pod 식별이다 (#418). spawn 되는 부하 pod
	// 의 ownerReference 로 부여되어 컨트롤러 삭제 시 Kubernetes GC 가 잔재를 회수한다.
	SelfPodName      string
	SelfPodNamespace string
	SelfPodUID       string
	// AllowedNamespaces 는 #420 의 허용 namespace 목록이다. 부하 대상 (spec.targetRef.namespace)
	// 과 부하 pod 생성 위치 (CR namespace) 가 목록 밖이면 run 을 거부한다. RBAC 의 namespace 한정
	// Role 과 같은 목록으로 배포되어 본 게이트가 1차, RBAC forbidden 이 최후 방어선이 된다.
	AllowedNamespaces []string
}

// defaultScoreTriggerMinInterval 은 spec.scoreTrigger.minInterval 이 비었을 때 적용 하는 debounce
// 기본값 이다. CRD default 와 동일 하며 직접 생성된 객체 의 zero 값 을 방어 한다.
const defaultScoreTriggerMinInterval = 10 * time.Minute

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

// Reconcile 은 비동기 state machine 의 단일 transition 을 처리 한다. blocking wait 없이 짧게 종료
// 하며 다음 transition 까지 RequeueAfter 로 controller-runtime workqueue 가 wait 한다.
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

	// prod cluster 차단 gate.
	if err := safety.CheckClusterLabel(ctx, r.K8sClient, r.AllowClusterLabel); err != nil {
		logger.Info("cluster label gate refused", "error", err.Error())
		setCondition(&ls, "Ready", metav1.ConditionFalse, "ClusterLabelGate", err.Error())
		if updateErr := r.Status().Update(ctx, &ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// #420 허용 namespace 게이트. 부하 pod 생성 위치 (CR namespace) 와 부하 대상
	// (targetRef.namespace) 이 허용 목록 밖이면 거부하고 카운터와 condition 으로 노출한다.
	// spec 변경 없이는 해소되지 않으므로 requeue 하지 않는다 (spec 변경 이벤트가 재평가를 유발).
	if err := safety.CheckNamespaceAllowed(r.AllowedNamespaces, ls.Namespace); err != nil {
		logger.Info("namespace gate refused spawn namespace", "error", err.Error())
		NamespaceDeniedTotal.WithLabelValues("spawn").Inc()
		setCondition(&ls, "Ready", metav1.ConditionFalse, "NamespaceDenied", "spawn: "+err.Error())
		if updateErr := r.Status().Update(ctx, &ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}
	if err := safety.CheckNamespaceAllowed(r.AllowedNamespaces, ls.Spec.TargetRef.Namespace); err != nil {
		logger.Info("namespace gate refused target namespace", "error", err.Error())
		NamespaceDeniedTotal.WithLabelValues("target").Inc()
		setCondition(&ls, "Ready", metav1.ConditionFalse, "NamespaceDenied", "target: "+err.Error())
		if updateErr := r.Status().Update(ctx, &ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	// suspend 검사. spec.suspend 또는 maxFailures 초과 시 skip. controller 는 spec 을 mutation 하지
	// 않으며 maxFailures 초과 상태 는 status condition 으로만 표현 한다.
	if r.isSuspended(&ls) {
		reason := "SpecSuspend"
		msg := "spec.suspend=true"
		if !ls.Spec.Suspend {
			reason = "MaxFailuresExceeded"
			msg = fmt.Sprintf("consecutiveFailures=%d >= maxFailures=%d", ls.Status.ConsecutiveFailures, ls.Spec.MaxFailures)
		}
		setCondition(&ls, "Suspended", metav1.ConditionTrue, reason, msg)
		if updateErr := r.Status().Update(ctx, &ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// schedule parse.
	schedule, err := r.CronParser.Parse(ls.Spec.Schedule)
	if err != nil {
		setCondition(&ls, "Ready", metav1.ConditionFalse, "InvalidSchedule", err.Error())
		if updateErr := r.Status().Update(ctx, &ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{}, nil
	}

	// state machine 분기.
	switch ls.Status.RunState {
	case injectorv1alpha1.RunStateRunning:
		return r.handleRunning(ctx, &ls, schedule)
	case injectorv1alpha1.RunStateAwaitingSpikeAlert:
		return r.handleAwaitingSpikeAlert(ctx, &ls, schedule)
	default:
		return r.handleIdle(ctx, &ls, schedule)
	}
}

// isSuspended 는 spec.suspend 또는 maxFailures 초과 상태를 판정 한다. 자동 suspend 는 status 변동
// 만으로 표현 하며 spec 을 mutation 하지 않아 GitOps drift 를 회피 한다.
func (r *LoadScenarioReconciler) isSuspended(ls *injectorv1alpha1.LoadScenario) bool {
	if ls.Spec.Suspend {
		return true
	}
	if ls.Spec.MaxFailures > 0 && ls.Status.ConsecutiveFailures >= ls.Spec.MaxFailures {
		return true
	}
	return false
}

// handleIdle 은 Idle phase 의 transition 이다. schedule 따른 다음 run 시각 을 산정 해 due 면 부하
// 인가 를 start, 아니면 wait.
func (r *LoadScenarioReconciler) handleIdle(ctx context.Context, ls *injectorv1alpha1.LoadScenario, schedule cron.Schedule) (ctrl.Result, error) {
	now := r.Now()
	// scoreTrigger 가 설정 되면 schedule 은 직접 트리거 가 아닌 score 평가 poll 주기 로 해석 된다. 매
	// poll 마다 간섭 score 를 평가 해 임계 충족 + minInterval debounce 통과 시에만 run 으로 진행 한다.
	// 미충족 / 에러 / provider 부재 는 evaluateScoreTrigger 가 condition + requeue 로 처리 하고
	// proceed=false 를 반환 한다. scoreTrigger 가 없으면 기존 cron schedule gate 로 동작 한다.
	if ls.Spec.ScoreTrigger != nil {
		proceed, res, err := r.evaluateScoreTrigger(ctx, ls, schedule, now)
		if err != nil || !proceed {
			return res, err
		}
		// LastScoreTriggerTime 은 run 이 실제로 시작된 RunStateRunning 전환부 에서 기록 한다. startRun
		// 실패 / Forbid lock skip 으로 run 이 안 떴는데 debounce 가 걸리는 것을 막는다.
	} else {
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
	}

	startErr := r.startRun(ctx, ls, now)
	if startErr != nil {
		if errors.Is(startErr, errSkipForbidLockHeld) {
			// Forbid 정책 의 lock 충돌 은 skip 결과 로 분류. ConsecutiveFailures 미증가, LastSuccessfulRunTime
			// 미갱신. metrics result=skip.
			RecordReconcileResult(ls.Namespace, ls.Name, "skip", float64(time.Now().Unix()))
			if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			next := schedule.Next(r.Now()).Sub(r.Now())
			if next <= 0 {
				next = time.Minute
			}
			return ctrl.Result{RequeueAfter: next}, nil
		}
		// run start 실패 (safety gate / loadgen Start 등). 실패 카운트 +1.
		ls.Status.ConsecutiveFailures++
		setCondition(ls, "Scheduled", metav1.ConditionFalse, "RunStartFailed", startErr.Error())
		if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		RecordReconcileResult(ls.Namespace, ls.Name, "error", float64(time.Now().Unix()))
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// stress Pod 가 spawn 되었음. RunState=Running 으로 전환 후 duration 만큼 wait.
	scheduleTime := metav1.NewTime(now)
	ls.Status.LastScheduleTime = &scheduleTime
	ls.Status.RunStartTime = &scheduleTime
	if ls.Spec.ScoreTrigger != nil {
		// score 트리거 run 이 실제로 시작된 시점 만 debounce 기준 으로 기록 한다.
		ls.Status.LastScoreTriggerTime = &scheduleTime
	}
	ls.Status.RunState = injectorv1alpha1.RunStateRunning
	setCondition(ls, "Ready", metav1.ConditionTrue, "ReconcileOK", "controller is reconciling scenario")
	setCondition(ls, "Scheduled", metav1.ConditionTrue, "RunStarted", "stress Pod spawned, awaiting duration")
	ActiveCount.Inc()
	if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
		// status update 실패 시에도 controller-runtime 의 재시도 흐름에 위임. ActiveCount 는 다음
		// reconcile 의 cleanup 단계 에서 자연 정합 된다.
		return ctrl.Result{}, updateErr
	}
	return ctrl.Result{RequeueAfter: ls.Spec.Duration.Duration + 5*time.Second}, nil
}

// evaluateScoreTrigger 는 spec.scoreTrigger 의 score 조건 을 평가 한다. 임계 충족 + debounce 통과 시
// (true, _, nil) 을 반환 해 handleIdle 이 startRun 으로 진행 하게 하고, 그 외 (provider 부재 / threshold
// 파싱 실패 / 평가 에러 / 임계 미달 / debounce) 는 condition 과 status 를 갱신 한 뒤 (false, requeue, _) 를
// 반환 한다. schedule 은 다음 poll 까지 의 requeue 간격 산정 에만 쓰인다.
func (r *LoadScenarioReconciler) evaluateScoreTrigger(ctx context.Context, ls *injectorv1alpha1.LoadScenario, schedule cron.Schedule, now time.Time) (bool, ctrl.Result, error) {
	st := ls.Spec.ScoreTrigger
	nextPoll := func() ctrl.Result {
		d := schedule.Next(now).Sub(now)
		if d <= 0 {
			d = time.Minute
		}
		return ctrl.Result{RequeueAfter: d}
	}
	updateThen := func(res ctrl.Result) (bool, ctrl.Result, error) {
		if err := r.Status().Update(ctx, ls); err != nil {
			return false, ctrl.Result{}, err
		}
		return false, res, nil
	}

	if r.ScoreProvider == nil {
		setCondition(ls, "ScoreTriggered", metav1.ConditionUnknown, "NoScoreProvider", "score provider not configured; scoreTrigger inert")
		return updateThen(ctrl.Result{RequeueAfter: time.Minute})
	}

	threshold, perr := strconv.ParseFloat(strings.TrimSpace(st.ScoreThreshold), 64)
	if perr != nil || threshold < 0 || threshold > 1 {
		setCondition(ls, "ScoreTriggered", metav1.ConditionFalse, "InvalidThreshold", fmt.Sprintf("scoreThreshold %q must be a number in 0..1", st.ScoreThreshold))
		return updateThen(nextPoll())
	}

	// debounce 검사 는 MaxScore HTTP 호출 보다 먼저 한다. debounce window 동안 correlation 으로의
	// 불필요한 query 를 피한다.
	minInterval := st.MinInterval.Duration
	if minInterval <= 0 {
		minInterval = defaultScoreTriggerMinInterval
	}
	if ls.Status.LastScoreTriggerTime != nil && now.Sub(ls.Status.LastScoreTriggerTime.Time) < minInterval {
		setCondition(ls, "ScoreTriggered", metav1.ConditionFalse, "Debounced",
			fmt.Sprintf("within minInterval %s of last trigger", minInterval))
		return updateThen(nextPoll())
	}

	victim := PodRef{Namespace: st.VictimRef.Namespace, Pod: st.VictimRef.Name}
	suspect := PodRef{Namespace: ls.Spec.TargetRef.Namespace, Pod: ls.Spec.TargetRef.Name}
	if st.SuspectRef != nil {
		suspect = PodRef{Namespace: st.SuspectRef.Namespace, Pod: st.SuspectRef.Name}
	}

	score, found, serr := r.ScoreProvider.MaxScore(ctx, victim, suspect, st.Dimension)
	if serr != nil {
		setCondition(ls, "ScoreTriggered", metav1.ConditionUnknown, "ScoreEvalError", serr.Error())
		// 일시적 조회 실패 는 scenario 실패 로 보지 않고 짧게 재시도 한다.
		return updateThen(ctrl.Result{RequeueAfter: 30 * time.Second})
	}
	if !found || score < threshold {
		setCondition(ls, "ScoreTriggered", metav1.ConditionFalse, "BelowThreshold",
			fmt.Sprintf("max score %.3f < threshold %.3f (found=%t)", score, threshold, found))
		return updateThen(nextPoll())
	}

	setCondition(ls, "ScoreTriggered", metav1.ConditionTrue, "ThresholdMet",
		fmt.Sprintf("score %.3f >= threshold %.3f (victim=%s/%s suspect=%s/%s)", score, threshold, victim.Namespace, victim.Pod, suspect.Namespace, suspect.Pod))
	return true, ctrl.Result{}, nil
}

// handleRunning 은 Running phase 의 transition 이다. duration 경과 시 stress Pod cleanup + lock
// release 후 AwaitingSpikeAlert (spec.spikeAlertAssertion=true) 또는 Idle (false) 로 전환.
func (r *LoadScenarioReconciler) handleRunning(ctx context.Context, ls *injectorv1alpha1.LoadScenario, schedule cron.Schedule) (ctrl.Result, error) {
	if ls.Status.RunStartTime == nil {
		// 비정상 상태. Idle 로 강제 전환 후 다음 reconcile 에서 schedule 재산정.
		ls.Status.RunState = injectorv1alpha1.RunStateIdle
		if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{Requeue: true}, nil
	}
	runEnd := ls.Status.RunStartTime.Time.Add(ls.Spec.Duration.Duration)
	now := r.Now()
	if now.Before(runEnd) {
		return ctrl.Result{RequeueAfter: runEnd.Sub(now)}, nil
	}

	// run 종료. stress Pod cleanup + lock release.
	if err := r.cleanupStressPods(ctx, ls); err != nil {
		setCondition(ls, "Scheduled", metav1.ConditionFalse, "CleanupFailed", err.Error())
		if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	if err := r.forceReleaseLease(ctx, ls); err != nil && !apierrors.IsNotFound(err) {
		setCondition(ls, "Scheduled", metav1.ConditionFalse, "LeaseReleaseFailed", err.Error())
		if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}
	ActiveCount.Dec()

	if ls.Spec.SpikeAlertAssertion && r.SpikeAsserter != nil {
		// AwaitingSpikeAlert phase 진입. RunStartTime 을 polling 시작 시각 으로 재해석 한다.
		pollStart := metav1.NewTime(now)
		ls.Status.RunStartTime = &pollStart
		ls.Status.RunState = injectorv1alpha1.RunStateAwaitingSpikeAlert
		setCondition(ls, "SpikeAlertObserved", metav1.ConditionUnknown, "Polling", "spike alert polling window started")
		if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{RequeueAfter: SpikePollInterval}, nil
	}

	// spike polling 비활성. 즉시 Idle 전환 + success 기록.
	return r.markRunSuccess(ctx, ls, schedule, now)
}

// handleAwaitingSpikeAlert 은 AwaitingSpikeAlert phase 의 transition 이다. 매 호출 시 단일 query 로
// firing 시리즈 확인. hit 이면 즉시 Idle 전환, hit 없고 window 만료 시 Idle 전환 (SpikeAlertObserved=False).
func (r *LoadScenarioReconciler) handleAwaitingSpikeAlert(ctx context.Context, ls *injectorv1alpha1.LoadScenario, schedule cron.Schedule) (ctrl.Result, error) {
	now := r.Now()
	if ls.Status.RunStartTime == nil {
		ls.Status.RunState = injectorv1alpha1.RunStateIdle
		if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		return ctrl.Result{Requeue: true}, nil
	}
	pollStart := ls.Status.RunStartTime.Time
	pollDeadline := pollStart.Add(SpikePollWindow)

	// 매 호출 시 단일 query. PromSpikeAsserter.Observe 가 단일 query 만 수행 하도록 짧은 timeout
	// 으로 재사용 한다 (sinceRunEnd 인자 는 polling window 만료 검사 에 사용 되지 않으므로 임의값).
	queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// PromSpikeAsserter.Observe 의 PollWindow 와 PollEvery 가 짧게 설정 되면 단일 query 후 즉시
	// 반환 된다. NewPromSpikeAsserter 가 long polling 으로 설정 된 경우를 회피 하기 위해 ad-hoc
	// shortPollAsserter 가 적합 하지만 본 commit 에서는 기존 인터페이스 를 유지 하고 single-shot
	// 의미 를 PollWindow=0 으로 처리 한다.
	alerts, err := r.SpikeAsserter.Observe(queryCtx, pollStart)
	if err != nil {
		setCondition(ls, "SpikeAlertObserved", metav1.ConditionUnknown, "SpikeAssertError", err.Error())
		if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		if now.After(pollDeadline) {
			return r.markRunSuccess(ctx, ls, schedule, now)
		}
		return ctrl.Result{RequeueAfter: SpikePollInterval}, nil
	}
	if len(alerts) > 0 {
		ls.Status.LastObservedSpikeAlerts = alerts
		setCondition(ls, "SpikeAlertObserved", metav1.ConditionTrue, "SpikeAlertFiring", fmt.Sprintf("observed alerts=%v", alerts))
		return r.markRunSuccess(ctx, ls, schedule, now)
	}
	if now.After(pollDeadline) {
		setCondition(ls, "SpikeAlertObserved", metav1.ConditionFalse, "NoSpikeAlertFiring", "polling window expired with no firing alert")
		return r.markRunSuccess(ctx, ls, schedule, now)
	}
	return ctrl.Result{RequeueAfter: SpikePollInterval}, nil
}

// markRunSuccess 는 run 성공 종료 시 status 갱신 흐름 이다. Idle 로 전환, LastSuccessfulRunTime 갱신,
// ConsecutiveFailures 리셋, success metric 기록.
func (r *LoadScenarioReconciler) markRunSuccess(ctx context.Context, ls *injectorv1alpha1.LoadScenario, schedule cron.Schedule, now time.Time) (ctrl.Result, error) {
	successTime := metav1.NewTime(now)
	ls.Status.LastSuccessfulRunTime = &successTime
	ls.Status.RunState = injectorv1alpha1.RunStateIdle
	ls.Status.RunStartTime = nil
	ls.Status.ConsecutiveFailures = 0
	setCondition(ls, "Scheduled", metav1.ConditionTrue, "RunSucceeded", "run completed within duration")
	setCondition(ls, "Ready", metav1.ConditionTrue, "ReconcileOK", "controller is reconciling scenario")
	if updateErr := r.Status().Update(ctx, ls); updateErr != nil {
		return ctrl.Result{}, updateErr
	}
	RecordReconcileResult(ls.Namespace, ls.Name, "success", float64(time.Now().Unix()))
	next := schedule.Next(r.Now()).Sub(r.Now())
	if next <= 0 {
		next = time.Minute
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

// startRun 은 부하 인가 의 trigger 단계다. safety gate, concurrencyPolicy 분기, lock acquire,
// target Pod fetch, loadgen Start 까지 비차단 으로 수행 한다. Forbid 정책 의 lock 충돌 은
// errSkipForbidLockHeld sentinel 로 분류 되어 Reconcile 본체 에서 skip 결과 로 처리 된다.
func (r *LoadScenarioReconciler) startRun(ctx context.Context, ls *injectorv1alpha1.LoadScenario, startTime time.Time) error {
	kind := loadgen.Kind(ls.Spec.Kind)
	if err := safety.CheckDuration(ls.Spec.Duration.Duration); err != nil {
		return fmt.Errorf("safety CheckDuration: %w", err)
	}
	if err := safety.CheckIntensity(kind, ls.Spec.Intensity); err != nil {
		return fmt.Errorf("safety CheckIntensity: %w", err)
	}

	// concurrencyPolicy = Replace 의 경우 진행 중 lease 를 강제 해제 한 뒤 시도. lease 가 없으면
	// (NotFound) 정상 진행 한다 (첫 Replace 호출 시점 등).
	if ls.Spec.ConcurrencyPolicy == injectorv1alpha1.ConcurrencyReplace {
		if err := r.forceReleaseLease(ctx, ls); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("replace lease: %w", err)
		}
	}

	if _, err := safety.AcquireLock(ctx, r.K8sClient, r.LockNamespace,
		ls.Spec.TargetRef.Namespace, ls.Spec.TargetRef.Name, r.LockHolder,
		ls.Spec.Duration.Duration*2); err != nil {
		if ls.Spec.ConcurrencyPolicy == injectorv1alpha1.ConcurrencyForbid {
			scheduleTime := metav1.NewTime(startTime)
			ls.Status.LastScheduleTime = &scheduleTime
			setCondition(ls, "Scheduled", metav1.ConditionFalse, "ForbidLockHeld", err.Error())
			return errSkipForbidLockHeld
		}
		return fmt.Errorf("acquire lock: %w", err)
	}
	// AcquireLock 의 release func 는 본 시점 에 호출 하지 않는다. lease TTL=duration*2 만료 또는
	// Running phase 종료 시점 의 forceReleaseLease 호출 로 정리 된다 (비동기 흐름).

	// target Pod fetch 후 nodeName 추출. cpu / memory / gpu loadgen 이 stress Pod 를 동일 node 에
	// 강제 배치 하기 위해 Params.TargetNode 가 비어 있으면 안 된다.
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
		Owner: loadgen.SelfOwnerReference(r.SelfPodName, r.SelfPodNamespace, r.SelfPodUID, ls.Namespace),
	}
	if err := gen.Start(ctx, params); err != nil {
		// stress Pod start 실패 시 lock 해제. 다음 run 의 lock 충돌 회피.
		_ = r.forceReleaseLease(ctx, ls)
		return fmt.Errorf("loadgen Start: %w", err)
	}
	// gen.Stop 은 본 함수 에서 호출 하지 않는다. Running phase 종료 시점 의 cleanupStressPods 가
	// 라벨 매칭 으로 stress Pod 를 정리 한다.
	return nil
}

// handleDeletion 은 DeletionTimestamp 가 설정된 LoadScenario 의 cleanup 흐름이다. lease 해제 와
// stress Pod 가비지 수거 후 finalizer 를 제거 한다.
func (r *LoadScenarioReconciler) handleDeletion(ctx context.Context, ls *injectorv1alpha1.LoadScenario) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ls, FinalizerName) {
		return ctrl.Result{}, nil
	}
	if err := r.forceReleaseLease(ctx, ls); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
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
	var errs []error
	for _, p := range pods.Items {
		if delErr := r.K8sClient.CoreV1().Pods(ls.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			errs = append(errs, fmt.Errorf("delete stress pod %s/%s: %w", ls.Namespace, p.Name, delErr))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// setCondition 은 metav1.Condition 표준 helper 다.
func setCondition(ls *injectorv1alpha1.LoadScenario, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&ls.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
}
