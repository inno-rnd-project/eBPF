// Package exporter 는 workload-injector 가 부하 시작 / 종료 시점과 영향 정량화 결과를 Prometheus
// 메트릭으로 노출하는 Collector 와 self-health 메트릭이다. workload-injector 의 lifecycle 이 짧은
// Job 이라 PodMonitor 가 Pod 가 살아 있는 동안만 scrape 한다.
package exporter

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/injector/blastradius"
	"netobs/internal/injector/loadgen"
)

// activeLabels 는 injector_active 메트릭의 라벨 셋이다. target 단위 1 시계열만 emit 되어 카디널리티
// 가 낮다.
var activeLabels = []string{
	"target_namespace",
	"target_pod",
	"target_node",
	"kind",
}

// blastLabels 는 correlation_blast_radius_score 와 baseline / impact 메트릭의 공통 라벨 셋이다.
// victim 단위로 분기된다.
var blastLabels = []string{
	"target_namespace",
	"target_pod",
	"target_node",
	"victim_namespace",
	"victim_pod",
	"victim_pod_uid",
	"kind",
}

// BlastResult 는 단일 victim 의 blast radius 산출 결과다. exporter 가 snapshot 으로 보관해 scrape
// 시점에 메트릭으로 변환한다. target 측 식별자도 함께 보관해 한 BlastResult 만으로 라벨 셋이
// 완전해진다.
type BlastResult struct {
	TargetNamespace string
	TargetPod       string
	TargetNode      string
	Kind            loadgen.Kind
	Victim          blastradius.VictimCandidate
	Score           float64
	Status          blastradius.Status
	Baseline        float64
	Impact          float64
}

// Collector 는 injector_active 와 blast radius 4 종 메트릭을 단일 snapshot 에서 emit 한다.
// prometheus.Collector 인터페이스를 직접 구현해 snapshot atomic replace 로 stale series 가 코드
// 경로상 존재하지 않게 한다.
type Collector struct {
	mu sync.RWMutex
	// activeSnapshot 은 현재 부하 활성 상태를 표현한다. nil 일 때는 시계열 자체가 emit 되지 않는다.
	activeSnapshot *activeEntry
	// blastSnapshot 은 마지막 cycle 의 victim 별 blast radius 결과다.
	blastSnapshot []BlastResult

	activeDesc   *prometheus.Desc
	scoreDesc    *prometheus.Desc
	baselineDesc *prometheus.Desc
	impactDesc   *prometheus.Desc
}

type activeEntry struct {
	TargetNamespace string
	TargetPod       string
	TargetNode      string
	Kind            loadgen.Kind
	Value           float64
}

// NewCollector 는 metric desc 를 미리 만들어 둔 Collector 를 구성한다.
func NewCollector() *Collector {
	return &Collector{
		activeDesc: prometheus.NewDesc(
			"injector_active",
			"workload-injector 의 부하 활성 상태. 1 (활성) / 0 (비활성). 부하 종료 직후 reset 후 linger 동안 0 으로 유지되어 마지막 scrape 가 transition 을 정확히 잡도록 한다.",
			activeLabels, nil,
		),
		scoreDesc: prometheus.NewDesc(
			"correlation_blast_radius_score",
			"부하 윈도우 동안 victim latency 가 baseline 대비 얼마나 증가했는지를 0 ~ 1 정규화한 score. status=ok 인 결과만 emit 된다.",
			blastLabels, nil,
		),
		baselineDesc: prometheus.NewDesc(
			"injector_baseline_latency_seconds",
			"부하 시작 전 BASELINE_WINDOW 동안의 victim latency 평균. blast radius 산출의 입력 검증용.",
			blastLabels, nil,
		),
		impactDesc: prometheus.NewDesc(
			"injector_impact_latency_seconds",
			"부하 윈도우 동안의 victim latency 평균. blast radius 산출의 입력 검증용.",
			blastLabels, nil,
		),
	}
}

// SetActive 는 injector_active 메트릭을 새 값으로 설정한다. value=1 은 부하 활성, value=0 은 종료
// 직후 transition. injector main 의 lifecycle 흐름이 (Set 1) → 부하 진행 → (Set 0) → linger → Clear
// 로 호출한다.
func (c *Collector) SetActive(targetNamespace, targetPod, targetNode string, kind loadgen.Kind, value float64) {
	c.mu.Lock()
	c.activeSnapshot = &activeEntry{
		TargetNamespace: targetNamespace,
		TargetPod:       targetPod,
		TargetNode:      targetNode,
		Kind:            kind,
		Value:           value,
	}
	c.mu.Unlock()
}

// ClearActive 는 injector_active 시계열 자체를 emit 에서 제거한다.
func (c *Collector) ClearActive() {
	c.mu.Lock()
	c.activeSnapshot = nil
	c.mu.Unlock()
}

// ReplaceBlast 는 blast radius snapshot 을 새 결과로 교체한다. 입력 슬라이스는 호출 후에도 호출자가
// 수정 가능하도록 내부 복사본을 보관한다.
func (c *Collector) ReplaceBlast(results []BlastResult) {
	copied := append([]BlastResult(nil), results...)
	c.mu.Lock()
	c.blastSnapshot = copied
	c.mu.Unlock()
}

// Describe 는 prometheus.Collector 인터페이스를 만족한다.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.activeDesc
	ch <- c.scoreDesc
	ch <- c.baselineDesc
	ch <- c.impactDesc
}

// Collect 는 snapshot 의 active / blast 결과를 메트릭으로 emit 한다.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	active := c.activeSnapshot
	blast := c.blastSnapshot
	c.mu.RUnlock()

	if active != nil {
		ch <- prometheus.MustNewConstMetric(c.activeDesc, prometheus.GaugeValue, active.Value,
			active.TargetNamespace, active.TargetPod, active.TargetNode, string(active.Kind),
		)
	}
	for _, b := range blast {
		if b.Status != blastradius.StatusOK {
			continue
		}
		labels := []string{
			b.TargetNamespace, b.TargetPod, b.TargetNode,
			b.Victim.Namespace, b.Victim.Pod, b.Victim.PodUID,
			string(b.Kind),
		}
		ch <- prometheus.MustNewConstMetric(c.scoreDesc, prometheus.GaugeValue, b.Score, labels...)
		ch <- prometheus.MustNewConstMetric(c.baselineDesc, prometheus.GaugeValue, b.Baseline, labels...)
		ch <- prometheus.MustNewConstMetric(c.impactDesc, prometheus.GaugeValue, b.Impact, labels...)
	}
}

// Health 는 injector 자체의 동작 가시성을 위한 self-health 메트릭 셋이다. main 이 cycle 의 결과에
// 따라 본 필드들을 갱신한다.
type Health struct {
	InjectorDuration *prometheus.GaugeVec
	InjectorRuns     *prometheus.CounterVec
	InjectorErrors   *prometheus.CounterVec
}

// NewHealth 는 self-health 메트릭들을 reg 에 등록한 뒤 반환한다.
func NewHealth(reg prometheus.Registerer) *Health {
	h := &Health{
		InjectorDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "injector_duration_seconds",
			Help: "마지막 injection cycle 의 부하 walltime. start 부터 stop 까지 측정.",
		}, []string{"kind"}),
		InjectorRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "injector_runs_total",
			Help: "injector 호출의 누적 합계. status 라벨은 ok / error / skipped_low_baseline / skipped_no_samples / skipped_gate.",
		}, []string{"kind", "status"}),
		InjectorErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "injector_errors_total",
			Help: "injector 의 단계별 에러 누적. stage 라벨은 baseline_fetch / loadgen_start / loadgen_stop / impact_fetch / cleanup.",
		}, []string{"kind", "stage"}),
	}
	reg.MustRegister(h.InjectorDuration, h.InjectorRuns, h.InjectorErrors)
	return h
}

// RecordDuration 은 부하 walltime 을 기록한다.
func (h *Health) RecordDuration(kind loadgen.Kind, d time.Duration) {
	h.InjectorDuration.WithLabelValues(string(kind)).Set(d.Seconds())
}

// RecordRun 은 injection 결과를 status 별 카운터로 누적한다.
func (h *Health) RecordRun(kind loadgen.Kind, status string) {
	h.InjectorRuns.WithLabelValues(string(kind), status).Inc()
}

// RecordError 는 단계별 에러를 누적한다.
func (h *Health) RecordError(kind loadgen.Kind, stage string) {
	h.InjectorErrors.WithLabelValues(string(kind), stage).Inc()
}
