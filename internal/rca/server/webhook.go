package server

import (
	"encoding/json"
	"log"
	"net/http"

	rcametrics "netobs/internal/rca/metrics"
	"netobs/internal/rca/registry"
	"netobs/internal/rca/store"
)

// alertmanagerPayload 는 Alertmanager webhook v4 payload 의 본 패키지가 사용하는 필드만 추린
// 부분 view 다. version / groupLabels 등은 무시한다.
type alertmanagerPayload struct {
	Alerts []alertmanagerAlert `json:"alerts"`
}

type alertmanagerAlert struct {
	Status string            `json:"status"`
	Labels map[string]string `json:"labels"`
}

// NewWebhookHandler 는 POST /webhook 핸들러를 만든다. payload 의 firing 알람만 처리하고
// resolved 알람은 emit 없이 200 으로 ack 한다. mapping 미등록 alert 는 raw labels echo back 한
// RCASummary 를 store 에 그대로 보관해 silent drop 을 회피한다.
func NewWebhookHandler(reg *registry.Registry, src registry.Sources, st *store.Store, met *rcametrics.Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p alertmanagerPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid payload: "+err.Error(), http.StatusBadRequest)
			return
		}

		var processed int
		for _, a := range p.Alerts {
			if a.Status != "firing" {
				continue
			}
			alertname := a.Labels["alertname"]
			if alertname == "" {
				continue
			}
			summary, _ := reg.Dispatch(alertname, a.Labels, src)
			st.Set(summary)
			met.Record(summary)
			processed++
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"received":  len(p.Alerts),
			"processed": processed,
		}); err != nil {
			log.Printf("webhook response encode: %v", err)
		}
	})
}

// NewRCAHandler 는 GET /rca?alert=<name> 핸들러를 만든다. store 에 alert entry 가 있으면 JSON
// 으로 응답하고, 없으면 404 를 돌려준다. alert query param 이 빈 값이면 store 의 전체 entry 를
// 배열로 응답한다 (운영자 대시보드 진단 용).
func NewRCAHandler(st *store.Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alertname := r.URL.Query().Get("alert")
		w.Header().Set("Content-Type", "application/json")

		if alertname == "" {
			if err := json.NewEncoder(w).Encode(st.All()); err != nil {
				log.Printf("rca all encode: %v", err)
			}
			return
		}
		entry, ok := st.Get(alertname)
		if !ok {
			http.Error(w, "no summary for alert "+alertname, http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(entry); err != nil {
			log.Printf("rca entry encode: %v", err)
		}
	})
}
