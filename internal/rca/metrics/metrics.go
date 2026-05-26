// Package metrics 는 rca-summarizer 의 Prometheus 메트릭 2 종을 정의하고 emit helper 를 노출한다.
// cardinality 가드는 본 패키지의 핵심 책임이다. last_summary_info 는 alert 당 1 시리즈로 collapse
// 되어 emit 시점마다 이전 라벨 셋을 명시적으로 Delete 한 뒤 새 라벨 셋으로 1 을 Set 한다.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/rca/registry"
)

// Metrics 는 본 패키지의 두 메트릭과 라벨 셋 캐시를 묶는다. emit 호출이 동시 발생해도 라벨 셋
// 교체 시점의 race 가 없도록 mu 로 직렬화한다.
type Metrics struct {
	emitted    *prometheus.CounterVec
	lastInfo   *prometheus.GaugeVec
	mu         sync.Mutex
	lastLabels map[string]prometheus.Labels // alertname → 가장 최근 emit 한 라벨 셋
}

// New 는 두 메트릭을 만들어 반환한다. Register 는 호출 측이 별도로 한다 (테스트 격리 용이).
func New() *Metrics {
	return &Metrics{
		emitted: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rca_summary_emitted_total",
				Help: "Cumulative count of RCA summaries composed per alert name. Increments on every Alertmanager webhook receipt that maps to a registered alert.",
			},
			[]string{"alert_name"},
		),
		lastInfo: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "rca_summary_last_summary_info",
				Help: "Most recent RCA summary per alert name exposed as an info-pattern gauge (value=1). Labels carry the dominant dimension, top suspect, and primary drop flow. Each alert name holds exactly one series at a time so that the cardinality is bounded by the registered alert count.",
			},
			[]string{"alert_name", "dominant_dimension", "top_suspect", "primary_drop_flow"},
		),
		lastLabels: make(map[string]prometheus.Labels),
	}
}

// Collectors 는 prometheus.Registerer.MustRegister 에 전달할 collector 슬라이스를 돌려준다.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.emitted, m.lastInfo}
}

// Record 는 한 RCASummary 가 산출됐을 때 호출된다. emitted_total 을 증가시키고 last_summary_info
// 의 이전 라벨 셋을 삭제한 뒤 새 라벨 셋으로 1 을 set 해 cardinality 가 alert 당 1 시리즈로
// 유지된다.
func (m *Metrics) Record(summary registry.RCASummary) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.emitted.WithLabelValues(summary.AlertName).Inc()

	newLabels := prometheus.Labels{
		"alert_name":         summary.AlertName,
		"dominant_dimension": defaultStr(summary.DominantDimension, "unknown"),
		"top_suspect":        defaultStr(summary.TopSuspect, "unknown"),
		"primary_drop_flow":  defaultStr(summary.PrimaryDropFlow, "none"),
	}

	if prev, ok := m.lastLabels[summary.AlertName]; ok {
		// 이전 라벨 셋과 정확히 동일하면 Delete 후 재등록 비용을 회피한다.
		if labelsEqual(prev, newLabels) {
			m.lastInfo.With(prev).Set(1)
			return
		}
		m.lastInfo.Delete(prev)
	}
	m.lastInfo.With(newLabels).Set(1)
	m.lastLabels[summary.AlertName] = newLabels
}

// defaultStr 은 empty 문자열을 라벨에 그대로 흘리지 않도록 sentinel 로 치환한다. Prometheus
// 라벨은 빈 문자열이 허용되지만 본 메트릭은 운영자가 dashboard 에서 즉시 의미를 파악해야 해
// 명시적 sentinel 이 가독성에 유리하다.
func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func labelsEqual(a, b prometheus.Labels) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		if b[k] != va {
			return false
		}
	}
	return true
}
