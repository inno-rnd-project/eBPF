package controller

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	injectorv1alpha1 "netobs/api/v1alpha1"
)

// #102 controller self-observability metric 3 종. controller-runtime 의 manager 가 노출 하는 기본
// metric (workqueue depth 등) 외에 LoadScenario 단위 reconcile 결과 와 active count 와 마지막 run
// 상태 를 직접 노출 해 운영자 가 dashboard 와 alert 에서 reconciler 상태 를 가시화 가능 하다.
var (
	// ReconcileTotal 은 reconcile 호출 결과 누계 다. result 라벨 (success / error / skip) 별로
	// 카운트 한다.
	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "loadscenario_reconcile_total",
			Help: "LoadScenario reconcile 호출 누계 (result 라벨 별 success/error/skip).",
		},
		[]string{"result"},
	)

	// ActiveCount 는 현재 run 진행 중 (RunState=Running) 인 LoadScenario 개수 다. controller worker
	// stall 감지 의 입력 신호. #459 부터 전이 경로마다 Inc/Dec 하지 않고 scrape 시점 에 매니저 캐시 의
	// Running CR 수를 센다. 종전 Inc/Dec 는 status 영속화 전에 수행되어 update 충돌 (상향 드리프트),
	// 전이 실패 후 재시도 (이중 감산), 실행 중 삭제와 비정상 분기 (감산 누락), controller 재시작 (음수)
	// 에서 실제와 어긋났다. 영속된 상태 만 세므로 모든 전이 경로 가 구조적 으로 정합 한다.
	ActiveCount = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "loadscenario_active_count",
			Help: "현재 run 진행 중 (RunState=Running) 인 LoadScenario 개수. scrape 시점 에 controller 캐시 의 CR 상태로 센다.",
		},
		countActiveRunning,
	)

	// LastRunStatus 는 LoadScenario 별 마지막 run 의 결과 indicator (0 / 1) 다. result 라벨 별로
	// 마지막 시점 의 상태 가 1 이면 그 외 는 0 으로 전환 된다.
	LastRunStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "loadscenario_last_run_status",
			Help: "LoadScenario 별 마지막 run 결과 indicator (0/1, result 라벨 success/error/skip).",
		},
		[]string{"loadscenario", "namespace", "result"},
	)

	// ReconcileTimestamp 는 controller 가 마지막 으로 reconcile 한 시각 의 unix timestamp 다.
	// LoadScenarioReconcilerStalled alert 가 time() - max(...) > 임계 로 stall 검출 에 사용 한다.
	ReconcileTimestamp = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "loadscenario_reconcile_timestamp_seconds",
			Help: "controller 의 마지막 reconcile 시각 (unix seconds).",
		},
	)
)

func init() {
	// controller-runtime 의 manager 가 노출 하는 metrics endpoint 에 본 메트릭 4 종 을 함께 등록.
	ctrlmetrics.Registry.MustRegister(ReconcileTotal, ActiveCount, LastRunStatus, ReconcileTimestamp, NamespaceDeniedTotal)
}

// NamespaceDeniedTotal 은 #420 의 허용 namespace 게이트 거부 계수다. scope 라벨은 target
// (spec.targetRef.namespace 거부) / spawn (CR namespace, 즉 부하 pod 생성 위치 거부) 으로
// 폐쇄된다. RBAC 는 최후 방어선이고 본 게이트가 1차 방어라, 거부가 조용히 requeue 루프로
// 숨지 않도록 카운터와 CR condition (NamespaceDenied) 양쪽에 노출한다.
var NamespaceDeniedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "loadscenario_namespace_denied_total",
		Help: "LoadScenario runs refused by the allowed-namespace gate (#420). scope=target means spec.targetRef.namespace is outside INJECTOR_ALLOWED_TARGET_NAMESPACES; scope=spawn means the CR namespace (where stress pods would spawn) is outside the list.",
	},
	[]string{"scope"},
)

// RecordReconcileResult 는 reconcile 단계 종료 시 ReconcileTotal 과 LastRunStatus 와 Reconcile
// Timestamp 를 일괄 갱신 하는 helper 다. result 는 "success" / "error" / "skip" 중 하나.
func RecordReconcileResult(namespace, loadscenario, result string, ts float64) {
	ReconcileTotal.WithLabelValues(result).Inc()
	ReconcileTimestamp.Set(ts)
	for _, r := range []string{"success", "error", "skip"} {
		v := float64(0)
		if r == result {
			v = 1
		}
		LastRunStatus.WithLabelValues(loadscenario, namespace, r).Set(v)
	}
}

// activeCountReader 는 ActiveCount 가 Running CR 을 셀 때 쓰는 reader 다. SetupWithManager 가 매니저
// 의 캐시 기반 client 를 주입한다. scrape goroutine 과 동시 접근 하므로 atomic 으로 둔다.
var activeCountReader atomic.Pointer[client.Reader]

// setActiveCountReader 는 ActiveCount 의 집계 대상 reader 를 지정한다.
func setActiveCountReader(r client.Reader) {
	activeCountReader.Store(&r)
}

// countActiveRunning 은 RunState=Running 인 LoadScenario 수를 센다. reader 미주입 (테스트, 기동 전)
// 이면 0 이고, 조회 실패 (캐시 미동기화 등) 면 실제와 다른 값을 내지 않도록 NaN 을 돌려준다.
func countActiveRunning() float64 {
	rp := activeCountReader.Load()
	if rp == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var list injectorv1alpha1.LoadScenarioList
	if err := (*rp).List(ctx, &list); err != nil {
		return math.NaN()
	}
	n := 0
	for i := range list.Items {
		if list.Items[i].Status.RunState == injectorv1alpha1.RunStateRunning {
			n++
		}
	}
	return float64(n)
}
