package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// IncidentsHandler 는 #236 의 alert 발화 이력 API 다. Prometheus 가 자동 기록하는 ALERTS 시계열을
// range 합성해 "언제 무슨 이벤트가 났었는지" 의 시간축을 별도 적재 없이 노출한다. events 가 현재
// 스냅샷을 다루는 것과 상보적이며, 각 항목의 발화 시각은 synthesis API 의 at 파라미터와 결합해
// 사건 시점 재구성의 진입점이 된다.
type IncidentsHandler struct {
	fetcher correlation.Fetcher
}

// NewIncidentsHandler 는 range query fetcher 를 주입받는다. fetcher 가 nil 이면 빈 응답을 graceful
// 하게 돌려준다.
func NewIncidentsHandler(fetcher correlation.Fetcher) *IncidentsHandler {
	return &IncidentsHandler{fetcher: fetcher}
}

// Register 는 /api/v1/incidents 라우트를 mux 에 등록한다.
func (h *IncidentsHandler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/incidents", apicommon.Chain(
		http.HandlerFunc(h.GetIncidents),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// IncidentsResponse 는 GET /api/v1/incidents 의 typed 응답이다.
type IncidentsResponse struct {
	GeneratedAt string     `json:"generated_at"`
	Range       string     `json:"range"`
	Step        string     `json:"step"`
	Incidents   []Incident `json:"incidents"`
	Summary     string     `json:"summary"`
}

// Incident 는 한 alert 발화 에피소드다. 동일 alert 시리즈라도 샘플 간극으로 분리된 재발화는 별개
// 에피소드가 된다. StartsAt 은 RFC3339 라 synthesis API 의 at 파라미터에 그대로 넣어 발화 시점의
// 상태를 재구성할 수 있다. 조회 range 시작 이전부터 발화 중이던 에피소드는 StartsAt 이 range 시작
// 으로 절단되며 truncated=true 로 표시한다.
type Incident struct {
	Alertname string            `json:"alertname"`
	Severity  string            `json:"severity,omitempty"`
	Component string            `json:"component,omitempty"`
	Status    string            `json:"status"`
	StartsAt  string            `json:"starts_at"`
	EndsAt    string            `json:"ends_at,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// incidentDropLabels 는 응답 라벨에서 제외할 라벨이다. 식별에 무의미한 scrape 계열과 이미 전용
// 필드로 승격된 라벨을 걸러 응답을 좁힌다.
var incidentDropLabels = map[string]bool{
	"__name__": true, "alertstate": true, "alertname": true, "severity": true, "component": true,
	"container": true, "endpoint": true, "instance": true, "job": true, "service": true,
}

// GetIncidents godoc
// @Summary      alert 발화 이력
// @Description  Prometheus 의 ALERTS 시계열을 range 합성해 기간 내 alert 발화 이력을 돌려준다. 동일 alert 의 재발화는 샘플 간극으로 별개 에피소드로 분리되고, range 끝까지 발화 중이면 status=firing, 중간에 끊겼으면 status=resolved 와 종료 시각이 채워진다. starts_at 은 synthesis API 의 at 파라미터에 그대로 넣어 발화 시점 상태를 재구성하는 진입점이다.
// @Tags         interference
// @Produce      json
// @Param        range  query  string  false  "조회 기간 (예: 1h, 6h, 최대 24h, 기본 1h)"
// @Param        step   query  string  false  "샘플 간격 (예: 1m, 최소 30s, 기본 1m)"
// @Param        limit  query  int     false  "상위 N 에피소드 (1-200, 기본 50)"
// @Success      200  {object}  IncidentsResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/incidents [get]
func (h *IncidentsHandler) GetIncidents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rng, err := parseDurationParam(q.Get("range"), time.Hour, 24*time.Hour)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_range", "range 파싱 실패: "+err.Error())
		return
	}
	step, err := parseDurationParam(q.Get("step"), time.Minute, time.Hour)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_step", "step 파싱 실패: "+err.Error())
		return
	}
	if step < 30*time.Second {
		step = 30 * time.Second
	}
	limit := 50
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}

	resp := IncidentsResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Range:       rng.String(),
		Step:        step.String(),
		Incidents:   []Incident{},
	}

	if h.fetcher != nil {
		end := time.Now()
		start := end.Add(-rng)
		series, err := h.fetcher.Fetch(r.Context(), `ALERTS{alertstate="firing"}`, start, end, step)
		if err != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", "Prometheus range 쿼리 실행 실패: "+err.Error())
			return
		}
		resp.Incidents = buildIncidents(series, start, end, step, limit)
	}

	resp.Summary = buildIncidentsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildIncidents 는 ALERTS 시리즈들을 발화 에피소드로 분해한다. 연속 샘플 간격이 2*step 을 넘으면
// 발화가 끊겼다 재발한 것으로 보고 에피소드를 분리한다. 마지막 샘플이 range 끝의 2*step 이내면
// 여전히 발화 중 (firing) 으로, 아니면 해소 (resolved) 로 판정한다.
func buildIncidents(series []correlation.LabeledSeries, start, end time.Time, step time.Duration, limit int) []Incident {
	gap := 2 * step
	out := []Incident{}
	for _, ls := range series {
		labels := ls.Series.Labels
		samples := ls.Series.Samples
		if len(samples) == 0 {
			continue
		}
		epStart := time.UnixMilli(samples[0].TimestampMs)
		prev := epStart
		flush := func(last time.Time) {
			inc := Incident{
				Alertname: labels["alertname"],
				Severity:  labels["severity"],
				Component: labels["component"],
				StartsAt:  epStart.UTC().Format(time.RFC3339),
				Labels:    filterIncidentLabels(labels),
			}
			// range 시작 직후 첫 샘플은 그 이전부터 발화 중이었을 수 있어 절단 표시한다.
			if !epStart.After(start.Add(gap)) {
				inc.Truncated = true
			}
			if last.After(end.Add(-gap)) {
				inc.Status = "firing"
			} else {
				inc.Status = "resolved"
				inc.EndsAt = last.UTC().Format(time.RFC3339)
			}
			out = append(out, inc)
		}
		for _, s := range samples[1:] {
			t := time.UnixMilli(s.TimestampMs)
			if t.Sub(prev) > gap {
				flush(prev)
				epStart = t
			}
			prev = t
		}
		flush(prev)
	}
	// 최근 발화 우선, 동률은 alertname 사전순으로 결정적 정렬.
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartsAt != out[j].StartsAt {
			return out[i].StartsAt > out[j].StartsAt
		}
		return out[i].Alertname < out[j].Alertname
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func filterIncidentLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if incidentDropLabels[k] || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildIncidentsSummary 는 발화 중 개수와 최근 에피소드를 한 줄로 적는다.
func buildIncidentsSummary(r IncidentsResponse) string {
	if len(r.Incidents) == 0 {
		return fmt.Sprintf("기간 내 alert 발화 없음 (%s)", r.Range)
	}
	firing := 0
	for _, in := range r.Incidents {
		if in.Status == "firing" {
			firing++
		}
	}
	latest := r.Incidents[0]
	return fmt.Sprintf("에피소드 %d개 (발화 중 %d), 최근 %s (%s, %s 시작)", len(r.Incidents), firing, latest.Alertname, latest.Status, latest.StartsAt)
}
