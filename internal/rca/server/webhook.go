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

// MaxWebhookPayloadBytes 는 단일 Alertmanager webhook payload 의 상한이다. 1 MiB 면 Alertmanager
// 가 group_interval 단위로 burst 발송하는 일반 케이스의 수십 배라 정상 운영에는 영향이 없고,
// 비정상 대용량 payload 가 본 프로세스의 메모리를 점유하는 케이스를 차단한다.
const MaxWebhookPayloadBytes = 1 << 20

// NewWebhookHandler 는 POST /webhook 핸들러를 만든다. payload 의 firing 알람만 처리하고
// resolved 알람은 emit 없이 200 으로 ack 한다. mapping 미등록 alert 는 store 에는 raw labels
// echo back 한 RCASummary 를 그대로 보관해 silent drop 을 회피하지만, metrics 에는 emit 하지
// 않아 등록 alert 9 종으로 라벨 카디널리티가 폐쇄된다.
func NewWebhookHandler(reg *registry.Registry, src registry.Sources, st *store.Store, met *rcametrics.Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p alertmanagerPayload
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxWebhookPayloadBytes)).Decode(&p); err != nil {
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
			summary, ok := reg.Dispatch(alertname, a.Labels, src)
			// store.Set 의 두 번째 인자 (registered) 에 ok 를 전달한다. 등록 alert 9 종은 cap
			// 무관하게 항상 store 에 자리가 보장되어 적대적 webhook 으로 미등록 alertname 이 cap
			// 을 채워도 핵심 alert 의 진단 흐름이 차단되지 않는다. 미등록 alert 의 store.Set 두
			// 번째 반환값이 false 면 cap 초과로 silent drop 된 케이스다.
			st.Set(summary, ok)
			if ok {
				// mapping 등록 alert (9 종) 만 metrics 에 emit 해 alert_name 라벨 카디널리티 폐쇄성
				// 을 보장한다. 외부에서 임의 alertname 으로 webhook 이 도달해도 메트릭 시리즈가
				// 폭증하지 않는다. 미등록 alert 의 진단 흐름은 /rca endpoint 의 store entry 로 유지.
				met.Record(summary)
			}
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
