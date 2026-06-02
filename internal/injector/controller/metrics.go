package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
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

	// ActiveCount 는 현재 reconcile 진행 중 인 LoadScenario 개수 (blocking duration 안) 다. controller
	// worker stall 감지 의 입력 신호.
	ActiveCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "loadscenario_active_count",
			Help: "현재 reconcile 진행 중 인 LoadScenario 개수.",
		},
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
	ctrlmetrics.Registry.MustRegister(ReconcileTotal, ActiveCount, LastRunStatus, ReconcileTimestamp)
}

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
