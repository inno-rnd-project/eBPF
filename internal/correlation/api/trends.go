package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// TrendsHandler 는 correlation-exporter 가 이미 emit 하는 correlation_* 시계열을 range query 로 읽어
// "간섭 강도 추이" 를 서빙한다. frontend 가 Prometheus 를 직접 호출하지 않고 exporter REST API 로
// 이력을 받게 한다. 적재 자체는 collector 가 매 scrape correlation_* gauge 로 이미 수행하며, 본
// 핸들러는 그 시계열을 range 로 노출만 한다.
type TrendsHandler struct {
	fetcher correlation.Fetcher
}

// NewTrendsHandler 는 range query fetcher 를 주입받는다. fetcher 가 nil 이면 빈 응답을 graceful 하게
// 돌려준다.
func NewTrendsHandler(fetcher correlation.Fetcher) *TrendsHandler {
	return &TrendsHandler{fetcher: fetcher}
}

// Register 는 /api/v1/trends 라우트를 mux 에 등록한다.
func (h *TrendsHandler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/trends", apicommon.Chain(
		http.HandlerFunc(h.GetTrends),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// trendSignals 는 노출 가능한 추이 신호의 화이트리스트다. 임의 PromQL 을 받지 않아 injection 과
// cardinality 를 동시에 통제한다. 모두 cluster 단위로 aggregate 해 신호당 1 시리즈로 bound 된다.
// #214 자원 사용량 / 지연 추이 5종 추가. latency_p99 는 send/recv 전 stage 혼합의 클러스터 p99 로
// 커널 네트워크 지연의 전반 추이를 나타내고, 대역폭은 same-node 트래픽이 egress 와 ingress 양쪽에
// 계상되는 이중 합산을 피해 방향별 2 신호로 분리한다 (layer=l4 는 syscall 관점). pressure_max 는
// 기존 recording rule 을 재사용하므로 promtool 검증 대상 신규 rule 이 없다.
var trendSignals = map[string]string{
	"noisy_neighbor_intensity": "max(correlation_noisy_neighbor_causal_strength)",
	"noisy_neighbor_count":     "count(correlation_noisy_neighbor_score >= 0.5)",
	"cross_node_intensity":     "max(correlation_cross_node_score)",
	"service_impact_intensity": "max(correlation_service_impact_score)",
	"latency_p99":              "histogram_quantile(0.99, sum by(le) (rate(netobs_stage_latency_labeled_seconds_bucket[5m])))",
	"drop_rate":                "sum(rate(netobs_drop_events_labeled_total[5m]))",
	"bandwidth_rx":             `sum(rate(netobs_pod_bytes_total{direction="ingress",layer="l4"}[5m]))`,
	"bandwidth_tx":             `sum(rate(netobs_pod_bytes_total{direction="egress",layer="l4"}[5m]))`,
	"pressure_max":             "max(node:pressure_score:5m)",
	"retrans_rate":             "sum(rate(netobs_retrans_events_labeled_total[5m]))",
	"srtt_max":                 "max(netobs_tcp_state_max_srtt_seconds)",
}

// trendSignalNames 는 화이트리스트 키를 정렬해 돌려준다. invalid_signal 에러 메시지가 시그널 추가
// 시 자동으로 최신 목록을 안내하게 한다.
func trendSignalNames() string {
	names := make([]string, 0, len(trendSignals))
	for k := range trendSignals {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, " / ")
}

// TrendsResponse 는 GET /api/v1/trends 의 typed 응답이다.
type TrendsResponse struct {
	GeneratedAt string        `json:"generated_at"`
	Signal      string        `json:"signal"`
	Range       string        `json:"range"`
	Step        string        `json:"step"`
	Series      []TrendSeries `json:"series"`
	Summary     string        `json:"summary"`
}

// TrendSeries 는 한 시계열의 라벨과 시점별 값이다.
type TrendSeries struct {
	Labels map[string]string `json:"labels,omitempty"`
	Points []TrendPoint      `json:"points"`
}

// TrendPoint 는 한 시점의 값이다.
type TrendPoint struct {
	TimestampMs int64   `json:"timestamp_ms"`
	Value       float64 `json:"value"`
}

// GetTrends godoc
// @Summary      진단 신호 추이
// @Description  진단 신호와 자원 사용량, 지연의 클러스터 추이를 range query 로 돌려준다. signal 은 화이트리스트(간섭 4종: noisy_neighbor_intensity / noisy_neighbor_count / cross_node_intensity / service_impact_intensity, 자원·지연 7종: latency_p99 / drop_rate / bandwidth_rx / bandwidth_tx / pressure_max / retrans_rate / srtt_max)만 허용해 임의 PromQL injection 과 cardinality 를 통제하며 신호당 1 시리즈로 bound 된다.
// @Tags         trends
// @Produce      json
// @Param        signal  query  string  true   "추이 신호 (noisy_neighbor_intensity / noisy_neighbor_count / cross_node_intensity / service_impact_intensity / latency_p99 / drop_rate / bandwidth_rx / bandwidth_tx / pressure_max / retrans_rate / srtt_max)"
// @Param        range   query  string  false  "조회 기간 (예: 1h, 6h, 최대 24h, 기본 1h)"
// @Param        step    query  string  false  "샘플 간격 (예: 5m, 최소 30s, 기본 5m)"
// @Success      200  {object}  TrendsResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/trends [get]
func (h *TrendsHandler) GetTrends(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	signal := strings.TrimSpace(q.Get("signal"))
	expr, ok := trendSignals[signal]
	if !ok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_signal", "signal 은 "+trendSignalNames()+" 중 하나여야 합니다")
		return
	}
	rng, err := parseDurationParam(q.Get("range"), time.Hour, 24*time.Hour)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_range", "range 파싱 실패: "+err.Error())
		return
	}
	step, err := parseDurationParam(q.Get("step"), 5*time.Minute, time.Hour)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_step", "step 파싱 실패: "+err.Error())
		return
	}
	if step < 30*time.Second {
		step = 30 * time.Second
	}

	resp := TrendsResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Signal:      signal,
		Range:       rng.String(),
		Step:        step.String(),
		Series:      []TrendSeries{},
	}

	if h.fetcher != nil {
		end := time.Now()
		start := end.Add(-rng)
		series, err := h.fetcher.Fetch(r.Context(), expr, start, end, step)
		if err != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", "Prometheus range 쿼리 실행 실패: "+err.Error())
			return
		}
		resp.Series = buildTrendSeries(series)
	}

	resp.Summary = buildTrendsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// parseDurationParam 은 duration 문자열을 파싱해 [0, max] 로 clamp 한다. 파싱 실패나 비양수면 def 를 쓴다.
func parseDurationParam(v string, def, max time.Duration) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive: %s", v)
	}
	if d > max {
		return max, nil
	}
	return d, nil
}

func buildTrendSeries(series []correlation.LabeledSeries) []TrendSeries {
	out := make([]TrendSeries, 0, len(series))
	for _, ls := range series {
		pts := make([]TrendPoint, 0, len(ls.Series.Samples))
		for _, s := range ls.Series.Samples {
			pts = append(pts, TrendPoint{TimestampMs: s.TimestampMs, Value: s.Value})
		}
		out = append(out, TrendSeries{Labels: ls.Series.Labels, Points: pts})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].Points) > len(out[j].Points) })
	return out
}

// buildTrendsSummary 는 시리즈 수와 최근 값을 한 줄로 적는다.
func buildTrendsSummary(r TrendsResponse) string {
	if len(r.Series) == 0 || len(r.Series[0].Points) == 0 {
		return fmt.Sprintf("추이 데이터 없음 (%s, %s)", r.Signal, r.Range)
	}
	last := r.Series[0].Points[len(r.Series[0].Points)-1]
	return fmt.Sprintf("%s 최근 값 %.3f (%s 추이, 시리즈 %d개)", r.Signal, last.Value, r.Range, len(r.Series))
}
