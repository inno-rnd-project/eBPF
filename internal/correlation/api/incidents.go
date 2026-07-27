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
	// Total 은 limit 적용 전 전체 에피소드 수, Truncated 는 잘렸는지다 (#352, 리스트 레벨). 개별
	// Incident 의 truncated (에피소드가 range 시작 이전부터 발화) 와는 다른 차원이다.
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Summary   string `json:"summary"`
}

// Incident 는 한 alert 발화 에피소드다. 동일 alert 시리즈라도 샘플 간극으로 분리된 재발화는 별개
// 에피소드가 된다. StartsAt 은 RFC3339 라 synthesis API 의 at 파라미터에 그대로 넣어 발화 시점의
// 상태를 재구성할 수 있다. 조회 range 시작 이전부터 발화 중이던 에피소드는 StartsAt 이 range 시작
// 으로 절단되며 truncated=true 로 표시한다.
type Incident struct {
	Alertname string `json:"alertname"`
	// Title 과 Summary 는 #349 의 사람이 읽을 설명이다. Title 은 alertname 의 한국어 제목이고
	// Summary 는 항목 labels 로 치환된 설명이다 (incidentCatalog). 카탈로그 미등록 alertname 은
	// Title 에 alertname 을 그대로 쓰고 Summary 를 생략한다 (신규 alert 도 incidents 가 안 깨짐).
	Title     string `json:"title,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Component string `json:"component,omitempty"`
	Status    string `json:"status"`
	// Scope 는 overview issues 와 동일 분류 (#326, #332) 로 pod / node / cluster 다. Node 와
	// Namespace 와 Pod 는 귀속 entity 라 프론트가 이슈에서 해당 화면으로 라우팅하는 입력이 되며,
	// cluster scope 는 라우팅 대상이 없어 전역 알림 목록에서 표시하는 것이 프론트 계약이다.
	Scope     string            `json:"scope,omitempty"`
	Node      string            `json:"node,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Pod       string            `json:"pod,omitempty"`
	StartsAt  string            `json:"starts_at"`
	EndsAt    string            `json:"ends_at,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// alertTargetsPod 는 alert 라벨이 해당 pod 를 가리키는지 판정한다 (#248). netobs/gpuobs 계열은
// src_pod, correlation 계열은 victim_pod, k8s 계열은 pod 라벨을 쓰므로 세 규약을 모두 본다.
// incidents 의 pod 필터와 node-map 의 pod-alert 매칭이 본 함수를 공유한다.
//
// #252 namespace 인지 매칭. 동일 이름 pod 가 여러 namespace 에 흔해 (Helm 차트, 공통 사이드카)
// pod 이름만 비교하면 한 namespace 의 alert 가 다른 namespace 의 동명 pod 에 잘못 붙는다. 규약
// 쌍별로 pod 라벨이 일치하고 그 쌍의 namespace 라벨이 존재하면 함께 일치해야 매칭한다. namespace
// 인자가 비면 제약하지 않아 incidents 의 pod 필터 (pod 파라미터만 받는 기존 계약) 가 유지되고,
// alert 에 해당 namespace 라벨이 없으면 pod 이름만으로 매칭해 정보 부족 시 보수적으로 잡는다.
func alertTargetsPod(labels map[string]string, namespace, pod string) bool {
	match := func(nsKey, podKey string) bool {
		if labels[podKey] != pod {
			return false
		}
		if namespace == "" {
			return true
		}
		ns := labels[nsKey]
		return ns == "" || ns == namespace
	}
	return match("namespace", "pod") || match("src_namespace", "src_pod") || match("victim_namespace", "victim_pod")
}

// alertTargetsNode 는 alert 라벨이 해당 node 를 가리키는지 판정한다 (#248).
func alertTargetsNode(labels map[string]string, node string) bool {
	return labels["node"] == node
}

// heartbeatAlert 는 상시 발화가 정상 상태라 활성 이슈가 아닌 heartbeat alert 판정이다 (#332).
// overview 의 issues 집계와 incidents 목록이 본 필터를 공유해 카드와 목록의 모수가 일치한다.
func heartbeatAlert(alertname string) bool {
	return alertname == "Watchdog"
}

// alertEntity 는 alert 라벨에서 귀속 entity (node 와 namespace 와 pod) 를 뽑는다 (#332). pod 계열
// 우선순위는 alertTargetsPod 의 규약 쌍 (pod/namespace, src_pod/src_namespace,
// victim_pod/victim_namespace) 순서를 따르며, 프론트가 이슈에서 해당 노드·pod 화면으로 라우팅하는
// 입력이 된다.
func alertEntity(labels map[string]string) (node, namespace, pod string) {
	node = labels["node"]
	for _, pair := range [][2]string{{"pod", "namespace"}, {"src_pod", "src_namespace"}, {"victim_pod", "victim_namespace"}} {
		if labels[pair[0]] != "" {
			return node, labels[pair[1]], labels[pair[0]]
		}
	}
	return node, "", ""
}

// incidentDropLabels 는 응답 라벨에서 제외할 라벨이다. 식별에 무의미한 scrape 계열과 이미 전용
// 필드로 승격된 라벨을 걸러 응답을 좁힌다.
var incidentDropLabels = map[string]bool{
	"__name__": true, "alertstate": true, "alertname": true, "severity": true, "component": true,
	"container": true, "endpoint": true, "instance": true, "job": true, "service": true,
	// #332 승격분 (scope / entity). src_pod 와 victim_pod 계열 원본 라벨은 어느 규약 쌍에서
	// 귀속됐는지를 보존하기 위해 남긴다 (승격 필드는 정규화된 값).
	"node": true, "namespace": true, "pod": true,
}

// GetIncidents godoc
// @Summary      alert 발화 이력
// @Description  Prometheus 의 ALERTS 시계열을 range 합성해 기간 내 alert 발화 이력을 돌려준다. 동일 alert 의 재발화는 샘플 간극으로 별개 에피소드로 분리되고, range 끝까지 발화 중이면 status=firing, 중간에 끊겼으면 status=resolved 와 종료 시각이 채워진다. starts_at 은 synthesis API 의 at 파라미터에 그대로 넣어 발화 시점 상태를 재구성하는 진입점이다. 상시 발화 heartbeat (Watchdog) 는 overview issues 와 공용 필터로 제외되어 발화 중 목록의 alertname dedup 수가 카드 total 과 일치한다 (#332). 각 항목의 scope (pod/node/cluster, overview 와 동일 분류) 와 귀속 entity (node 와 namespace 와 pod) 는 프론트가 이슈에서 해당 화면으로 라우팅하는 입력이며, cluster scope 는 라우팅 대상이 없어 전역 알림 목록에서 표시한다. title 과 summary 는 사람이 읽을 설명으로 (#349), title 은 alertname 의 한국어 제목이고 summary 는 항목 labels 로 치환된 설명이다. 카탈로그 미등록 alertname (kube-prometheus-stack 내장 alert 등) 은 title 에 alertname 을 그대로 쓰고 summary 를 생략한다.
// @Tags         interference
// @Produce      json
// @Param        range  query  string  false  "조회 기간 (예: 1h, 6h, 최대 24h, 기본 1h)"
// @Param        step   query  string  false  "샘플 간격 (예: 1m, 최소 30s, 기본 1m)"
// @Param        limit  query  int     false  "상위 N 에피소드 (1-200, 기본 50)"
// @Param        node   query  string  false  "node 라벨이 이 노드를 가리키는 에피소드만 조회"
// @Param        pod    query  string  false  "pod/src_pod/victim_pod 라벨이 이 pod 를 가리키는 에피소드만 조회"
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
	limit, ok := apicommon.ParseLimit(r, 50, 200)
	if !ok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit 은 정수여야 합니다")
		return
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
		// #332 heartbeat 제외. overview issues 집계와 같은 필터를 공유해 카드 total 과 목록의
		// 모수가 일치한다. node / pod 필터보다 앞서 전체 목록 기준으로 적용한다.
		series = filterIncidentSeries(series, func(labels map[string]string) bool {
			return !heartbeatAlert(labels["alertname"])
		})
		// #248 node / pod 필터. 에피소드 분해 전 시리즈 단계에서 걸러 limit 이 필터 후 집합에
		// 적용되게 한다.
		if node := strings.TrimSpace(q.Get("node")); node != "" {
			series = filterIncidentSeries(series, func(labels map[string]string) bool {
				return alertTargetsNode(labels, node)
			})
		}
		if pod := strings.TrimSpace(q.Get("pod")); pod != "" {
			series = filterIncidentSeries(series, func(labels map[string]string) bool {
				return alertTargetsPod(labels, "", pod)
			})
		}
		all := buildIncidents(series, start, end, step)
		resp.Total = len(all)
		if len(all) > limit {
			all = all[:limit]
			resp.Truncated = true
		}
		resp.Incidents = all
	}

	resp.Summary = buildIncidentsSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildIncidents 는 ALERTS 시리즈들을 발화 에피소드로 분해한다. 연속 샘플 간격이 2*step 을 넘으면
// 발화가 끊겼다 재발한 것으로 보고 에피소드를 분리한다. 마지막 샘플이 range 끝의 2*step 이내면
// 여전히 발화 중 (firing) 으로, 아니면 해소 (resolved) 로 판정한다.
func buildIncidents(series []correlation.LabeledSeries, start, end time.Time, step time.Duration) []Incident {
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
			node, namespace, pod := alertEntity(labels)
			// #349 사람이 읽을 title / summary. 원본 labels (filterIncidentLabels 전) 로 렌더해
			// map / reason 등 승격되지 않은 라벨도 치환에 쓴다.
			title, summary := incidentDescribe(labels["alertname"], labels)
			inc := Incident{
				Alertname: labels["alertname"],
				Title:     title,
				Summary:   summary,
				Severity:  labels["severity"],
				Component: labels["component"],
				Scope:     alertScope(labels),
				Node:      node,
				Namespace: namespace,
				Pod:       pod,
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
	// 최근 발화 우선, 동률은 alertname 사전순. 동일 alert 가 같은 시각에 라벨만 다르게 다건 발화하는
	// 실측 케이스가 있어, 안정 정렬로 Prometheus 의 라벨 기준 시리즈 순서를 보존해 응답 결정성을
	// 확보한다.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartsAt != out[j].StartsAt {
			return out[i].StartsAt > out[j].StartsAt
		}
		return out[i].Alertname < out[j].Alertname
	})
	return out
}

// filterIncidentSeries 는 라벨 술어를 만족하는 시리즈만 남긴다.
func filterIncidentSeries(series []correlation.LabeledSeries, match func(map[string]string) bool) []correlation.LabeledSeries {
	out := make([]correlation.LabeledSeries, 0, len(series))
	for _, ls := range series {
		if match(ls.Series.Labels) {
			out = append(out, ls)
		}
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
