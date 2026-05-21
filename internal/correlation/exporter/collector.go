// Package exporter 는 internal/correlation 의 산출 결과를 Prometheus 메트릭으로 노출한다.
// correlation-exporter 바이너리가 본 패키지의 Collector 와 Health 를 wire-up 하고 reconcile 루프로
// snapshot 을 주기적 교체한다.
package exporter

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/correlation"
)

// neighborLabels 는 correlation_noisy_neighbor_* 메트릭에 공통으로 부여되는 8개 라벨이다. victim 과
// suspect 양쪽의 (namespace, pod, pod_uid) 6 개에 resource_dimension 과 rank 가 추가된다. rank 는
// SelectTopN 의 TopN 가드로 1..N 범위라 카디널리티가 통제된다.
var neighborLabels = []string{
	"victim_namespace",
	"victim_pod",
	"victim_pod_uid",
	"suspect_namespace",
	"suspect_pod",
	"suspect_pod_uid",
	"resource_dimension",
	"rank",
}

// Collector 는 마지막 reconcile cycle 의 NoisyNeighbor snapshot 을 보관해 Prometheus scrape 시점에
// score 와 lag 메트릭으로 emit 한다. prometheus.Collector 인터페이스를 직접 구현해 snapshot 교체
// 시 GaugeVec.Reset() 패턴이 가질 race 위험을 차단하고 stale series 가 코드 경로상 존재하지 않게
// 한다.
type Collector struct {
	mu       sync.RWMutex
	snapshot []correlation.NoisyNeighbor
	// step 은 LagSteps 를 초 단위로 변환할 때 곱해진다. exporter 가 Correlator 의 Config.Step 과
	// 동일 값을 받아 lag step 의 시간 의미를 보존한다.
	step time.Duration

	scoreDesc  *prometheus.Desc
	lagDesc    *prometheus.Desc
	pvalueDesc *prometheus.Desc
}

// NewCollector 는 Prometheus scrape 시 emit 할 metric desc 두 개를 미리 만들어 두는 Collector 를
// 구성한다. step 은 lag step 의 시간 의미를 보존하기 위해 reconcile config 의 Step 과 동일 값을
// 전달한다.
func NewCollector(step time.Duration) *Collector {
	return &Collector{
		step: step,
		scoreDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_score",
			"Pearson 상관계수 최대 절대값. 1.0 에 가까울수록 suspect 자원 압박과 victim latency 가 강한 동조를 보인다.",
			neighborLabels, nil,
		),
		lagDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_lag_seconds",
			"score 가 최대 절대값을 보인 lag 의 초 단위 환산. 양수면 suspect 변동이 victim latency 를 N 초 선행하는 인과 방향이다.",
			neighborLabels, nil,
		),
		pvalueDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_pvalue",
			"#69 의 Granger causality p-value. src (suspect) 가 dst (victim latency) 를 Granger-cause 하는지의 통계적 유의성. 0.05 미만이면 high-confidence 인과 신호로 본다. continuous 값이라 라벨이 아닌 별개 메트릭으로 분리해 cardinality 폭증을 차단한다.",
			neighborLabels, nil,
		),
	}
}

// Replace 는 reconcile cycle 산출물로 snapshot 을 교체한다. 입력 슬라이스는 호출 후에도 호출자
// 측에서 수정 가능하도록 내부적으로 복사본을 보관한다.
func (c *Collector) Replace(neighbors []correlation.NoisyNeighbor) {
	copied := append([]correlation.NoisyNeighbor(nil), neighbors...)
	c.mu.Lock()
	c.snapshot = copied
	c.mu.Unlock()
}

// Describe 는 prometheus.Collector 인터페이스를 만족한다.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.scoreDesc
	ch <- c.lagDesc
	ch <- c.pvalueDesc
}

// Collect 는 현재 snapshot 의 모든 NoisyNeighbor 를 score / lag 두 메트릭으로 emit 한다. snapshot
// 이 nil 또는 빈 슬라이스면 series 를 0 개 emit 해 첫 reconcile 전 stale 0 값을 보내지 않는다.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snapshot := c.snapshot
	step := c.step
	c.mu.RUnlock()

	stepSeconds := step.Seconds()
	for _, n := range snapshot {
		rank := strconv.Itoa(n.Rank)
		labels := []string{
			n.Victim.Namespace,
			n.Victim.Pod,
			n.Victim.PodUID,
			n.Suspect.Namespace,
			n.Suspect.Pod,
			n.Suspect.PodUID,
			string(n.Dimension),
			rank,
		}
		ch <- prometheus.MustNewConstMetric(c.scoreDesc, prometheus.GaugeValue, n.Score, labels...)
		ch <- prometheus.MustNewConstMetric(c.lagDesc, prometheus.GaugeValue, float64(n.LagSteps)*stepSeconds, labels...)
		if n.GrangerOK {
			ch <- prometheus.MustNewConstMetric(c.pvalueDesc, prometheus.GaugeValue, n.PValue, labels...)
		}
	}
}

// Health 는 exporter 자체의 동작 가시성을 위한 self-health 메트릭 셋이다. reconcile 루프가 매 cycle
// 결과에 따라 본 필드들을 갱신한다.
type Health struct {
	ReconcileDuration         prometheus.Gauge
	ReconcilePairs            prometheus.Counter
	ReconcileNeighbors        prometheus.Counter
	ReconcileSkipped          *prometheus.CounterVec
	ReconcilePartial          prometheus.Counter
	ReconcileMetricsExpected  prometheus.Gauge
	ReconcileMetricsObserved  prometheus.Gauge
	LastSuccessTimestamp      prometheus.Gauge
	ReconcileErrors           prometheus.Counter
}

// NewHealth 는 self-health 메트릭들을 생성해 reg 에 등록한 뒤 반환한다.
func NewHealth(reg prometheus.Registerer) *Health {
	h := &Health{
		ReconcileDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_duration_seconds",
			Help: "마지막 reconcile cycle 의 소요 시간 (초). fetch + Pearson 산출 + Top-N 선택 전체 walltime.",
		}),
		ReconcilePairs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_pairs_total",
			Help: "Correlator.Correlate 가 산출한 페어의 누적 합계.",
		}),
		ReconcileNeighbors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_neighbors_total",
			Help: "SelectTopN 채택 후 메트릭으로 emit 된 noisy neighbor 엔트리의 누적 합계.",
		}),
		ReconcileSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "correlation_reconcile_skipped_total",
			Help: "산출에서 skip 된 페어의 누적 합계. reason 라벨은 Pearson status 분류 (low_samples, constant) 다.",
		}, []string{"reason"}),
		ReconcilePartial: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_partial_total",
			Help: "reconcile cycle 의 산출 결과에 등장한 distinct metric 수가 expected query 수보다 작아 일부 query 가 데이터를 만들지 못한 cycle 의 누적. 운영자는 본 카운터가 증가하면 PrometheusURL / query 문법 / 입력 recording rule 가용성을 점검한다.",
		}),
		ReconcileMetricsExpected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_metrics_expected",
			Help: "마지막 reconcile cycle 의 DefaultMetrics + ExtraMetrics 총 query 수.",
		}),
		ReconcileMetricsObserved: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_metrics_observed",
			Help: "마지막 reconcile cycle 의 결과에 등장한 distinct src/dst metric 수. expected 와 같지 않으면 일부 query 가 데이터를 만들지 못한 상태.",
		}),
		LastSuccessTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_last_success_timestamp_seconds",
			Help: "마지막 성공 reconcile 의 Unix epoch 초. CorrelationExporterStalled alert 의 입력.",
		}),
		ReconcileErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_errors_total",
			Help: "reconcile cycle 이 wrapped error 로 종료된 횟수의 누적 합계.",
		}),
	}
	reg.MustRegister(
		h.ReconcileDuration,
		h.ReconcilePairs,
		h.ReconcileNeighbors,
		h.ReconcileSkipped,
		h.ReconcilePartial,
		h.ReconcileMetricsExpected,
		h.ReconcileMetricsObserved,
		h.LastSuccessTimestamp,
		h.ReconcileErrors,
	)
	return h
}

// RecordCycle 은 reconcile cycle 1 회의 결과를 self-health 메트릭에 반영한다. results 와 neighbors
// 의 길이 차이는 SelectTopN 의 필터링 (latency 페어 외 dedup, dimension 미분류, topN 컷) 으로 발생
// 하며 RecordCycle 은 결과 길이만 기록하고 필터별 분해는 하지 않는다 (운영자는 pairs_total 과
// neighbors_total 의 비로 필터링 비율을 관측한다). expectedMetrics 는 Correlator 가 fetch 시도한
// query 총 수 (DefaultMetrics + ExtraMetrics) 다. 본 cycle 의 results 에 등장한 distinct metric
// 수와 비교해 partial fetch (일부 query 가 데이터를 만들지 못한 cycle) 여부를 판정한다.
func (h *Health) RecordCycle(duration time.Duration, results []correlation.CorrelationResult, neighbors []correlation.NoisyNeighbor, expectedMetrics int) {
	h.ReconcileDuration.Set(duration.Seconds())
	h.ReconcilePairs.Add(float64(len(results)))
	h.ReconcileNeighbors.Add(float64(len(neighbors)))
	// WithLabelValues 는 매 호출마다 라벨 해시 lookup 을 수행하므로 페어가 수천 개에 이르는 hot
	// path 에서 results 루프 안에 두면 비용이 누적된다. low_samples 와 constant 두 reason 은 enum
	// 으로 고정이라 루프 진입 전에 한 번 lookup 해 캐시한다.
	lowSamples := h.ReconcileSkipped.WithLabelValues("low_samples")
	constant := h.ReconcileSkipped.WithLabelValues("constant")
	observedMetrics := make(map[string]struct{})
	for _, r := range results {
		switch r.Status {
		case correlation.StatusSkippedLowSamples:
			lowSamples.Inc()
		case correlation.StatusSkippedConstant:
			constant.Inc()
		}
		// distinct metric 수 산출. EnumeratePairs 가 만든 양방향 페어이므로 Src 와 Dst 양측 모두
		// 집합에 넣어 dataset 가 emit 한 모든 unique query 를 셋다.
		observedMetrics[r.Pair.SrcMetric] = struct{}{}
		observedMetrics[r.Pair.DstMetric] = struct{}{}
	}
	observed := len(observedMetrics)
	h.ReconcileMetricsExpected.Set(float64(expectedMetrics))
	h.ReconcileMetricsObserved.Set(float64(observed))
	if expectedMetrics > 0 && observed < expectedMetrics {
		h.ReconcilePartial.Inc()
	}
	h.LastSuccessTimestamp.SetToCurrentTime()
}

// RecordError 는 reconcile cycle 이 error 로 종료됐을 때 호출된다. LastSuccessTimestamp 는 갱신하지
// 않아 CorrelationExporterStalled alert 가 의도대로 발화한다.
func (h *Health) RecordError() {
	h.ReconcileErrors.Inc()
}
