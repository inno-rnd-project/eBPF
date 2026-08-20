package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"netobs/internal/apicommon"
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

// MaxAlertsPerWebhook 은 webhook 1건이 dispatch 하는 firing alert 수 상한이다 (#419). payload
// 크기 (1 MiB) 는 제한돼 있어도 그 안의 alert 수는 무제한이라, alert 1건당 하류 조회 (Prometheus,
// correlation snapshot, gpuobs) 최대 3회가 상한 없이 증폭됐다. 등록 alert 이 수십 종이고 전 노드
// 동시 발화를 겹쳐도 firing 수십 건 수준이라 64 는 정상 운영을 검열하지 않으면서 하류 호출 수를
// 최대 64 x 3 으로 유계화한다. 초과분은 rca_webhook_alerts_dropped_total{reason="over_cap"} 으로
// 계수된다 (Alertmanager 는 200 을 받으므로 재전송하지 않는다).
const MaxAlertsPerWebhook = 64

// webhookDispatchWorkers 는 dispatch 동시성이다 (#419). 종전 직렬 루프는 alert 1건의 하류 조회가
// 최악 3 x fetchTimeout 이라 alert 가 수십 건이면 webhook-timeout (기본 30s) 을 초과했다. 4 워커는
// 요청 컨텍스트 전파 (취소 시 잔여 fail-fast) 와 결합해 처리 시간을 timeout 예산 안으로 당기면서
// 하류 (Prometheus / correlation-exporter) 에 가하는 동시 부하를 4 로 유계화한다.
const webhookDispatchWorkers = 4

// dedupeWindow 는 Alertmanager 재전송 멱등 창이다 (#419). Alertmanager 는 응답을 못 받으면 수초
// 간격으로 같은 payload 를 재전송하는데, 동일 alert (labels 지문 동일) 가 본 창 안에 다시 오면
// 하류 조회 없이 억제한다. 본 서비스의 실제 라우트 설정 (deploy/rca-summarizer/base/
// alertmanagerconfig.yaml: groupWait 15s, groupInterval 1m, repeatInterval 30m) 기준으로, 창을
// groupInterval 과 같게 두면 60초 주기의 정상 갱신 통보가 지터에 따라 경계에서 임의로 억제되므로
// 그 절반인 30s 로 둔다. 응답 실패 재전송 (수초 간격) 은 여전히 잡히고 정상 갱신 (60s 이상 간격)
// 은 항상 통과한다. alertmanagerconfig 의 주기를 줄이면 본 상수와의 간격을 함께 확인해야 한다.
const dedupeWindow = 30 * time.Second

// dedupeMaxKeys 는 멱등 캐시의 키 상한이다. 초과 시 전체를 비워 무제한 증가를 막는 backstop 으로,
// 정상 운영의 동시 고유 alert 수 (수십) 대비 충분히 크다.
const dedupeMaxKeys = 1024

// recentAlerts 는 최근 dispatch 한 alert 의 labels 지문 → 처리 시각 캐시다.
type recentAlerts struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// suppress 는 지문이 창 안에 있으면 true (억제) 를 돌려주고, 아니면 현재 시각으로 기록한다.
func (r *recentAlerts) suppress(fp string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]time.Time, 64)
	}
	if t, ok := r.seen[fp]; ok && now.Sub(t) < dedupeWindow {
		return true
	}
	if len(r.seen) >= dedupeMaxKeys {
		r.seen = make(map[string]time.Time, 64)
	}
	r.seen[fp] = now
	return false
}

// fingerprint 는 alert labels 의 결정적 지문이다. 키 정렬 후 key=value 를 이어 붙여 Alertmanager
// 재전송 (동일 labels) 이 같은 지문을 얻게 한다. 값에 구분자 (0x1f) 가 들어가는 극단 케이스의
// 충돌은 억제가 잘못돼도 dedupeWindow(30s) 뒤 자연 해소되는 무해한 방향이라 해시 없이 단순
// 결합으로 둔다.
func fingerprint(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte('\x1f')
	}
	return b.String()
}

// NewWebhookHandler 는 POST /webhook 핸들러를 만든다. payload 의 firing 알람만 처리하고
// resolved 알람은 emit 없이 200 으로 ack 한다. mapping 미등록 alert 는 store 에 AlertName 만
// 채운 RCASummary 로 보관해 도달 여부의 진단 표면을 남기고 (raw labels 는 보존하지 않는다),
// metrics 에는 emit 하지 않아 등록 alert 종으로 라벨 카디널리티가 폐쇄된다. confidenceThreshold 는 #122 의 false
// positive guard 임계 다. RCASummary.ConfidenceScore 가 본 값 미만 인 등록 alert 는 metrics emit
// 을 skip 하고 store 에는 그대로 보관 + skipped_total counter 만 증가 한다.
//
// #419 처리 통제 3종: firing alert 수 상한 (MaxAlertsPerWebhook), 유계 동시성 dispatch
// (webhookDispatchWorkers, 요청 예산 소진 시 잔여 fail-fast), Alertmanager 재전송 멱등 억제
// (dedupeWindow). 상한 초과 / 취소 / 중복은 rca_webhook_alerts_dropped_total 로 계수된다.
func NewWebhookHandler(reg *registry.Registry, src registry.Sources, st *store.Store, met *rcametrics.Metrics, confidenceThreshold float64) http.Handler {
	recent := &recentAlerts{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p alertmanagerPayload
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxWebhookPayloadBytes)).Decode(&p); err != nil {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_payload", "payload 파싱 실패: "+err.Error())
			return
		}

		// firing + alertname 보유 alert 만 후보로 거른 뒤 멱등 창과 상한을 차례로 적용한다.
		now := time.Now()
		var candidates []alertmanagerAlert
		var overCapNames []string
		var overCap, duplicates int
		for _, a := range p.Alerts {
			if a.Status != "firing" || a.Labels["alertname"] == "" {
				continue
			}
			if recent.suppress(fingerprint(a.Labels), now) {
				duplicates++
				continue
			}
			if len(candidates) >= MaxAlertsPerWebhook {
				overCap++
				overCapNames = append(overCapNames, a.Labels["alertname"])
				continue
			}
			candidates = append(candidates, a)
		}
		if duplicates > 0 {
			met.RecordWebhookDropped("duplicate", duplicates)
		}
		if overCap > 0 {
			met.RecordWebhookDropped("over_cap", overCap)
			// 누락 추적을 위해 어느 alert 가 빠졌는지 alertname 단위로 남긴다 (#419 리뷰). 표기는
			// 20개에서 자르며 계수는 dropped_total 이 담당한다.
			names := overCapNames
			if len(names) > 20 {
				names = append(append([]string(nil), names[:20]...), "...")
			}
			log.Printf("rca: webhook alert cap exceeded, dropped %d of %d firing alerts (dropped alertnames: %s)", overCap, overCap+len(candidates), strings.Join(names, ","))
		}

		// 유계 동시성 dispatch. 요청 컨텍스트가 끝나면 (클라이언트 취소 / 서버 WriteTimeout) 잔여
		// 후보를 dispatch 하지 않고 canceled 로 계수해, 죽은 요청의 하류 조회가 이어지지 않는다.
		ctx := r.Context()
		var (
			mu        sync.Mutex
			processed int
			canceled  int
		)
		sem := make(chan struct{}, webhookDispatchWorkers)
		var wg sync.WaitGroup
		for _, a := range candidates {
			if ctx.Err() != nil {
				canceled++
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(a alertmanagerAlert) {
				defer wg.Done()
				defer func() { <-sem }()
				alertname := a.Labels["alertname"]
				summary, ok := reg.Dispatch(ctx, alertname, a.Labels, src)
				// store.Set 의 두 번째 인자 (registered) 에 ok 를 전달한다. 등록 alert 는 cap
				// 무관하게 항상 store 에 자리가 보장되어 적대적 webhook 으로 미등록 alertname 이
				// cap 을 채워도 핵심 alert 의 진단 흐름이 차단되지 않는다. cap 초과 거부는 로그와
				// 카운터로 관측한다(#446). 종전에는 반환값을 버려 미등록 alert의 드롭이 무관측이라
				// 운영자가 webhook 도달 여부조차 확인할 수 없었다.
				if _, stored := st.Set(summary, ok); !stored {
					log.Printf("rca: store cap exceeded, unregistered alert %s dropped (entries=%d)", alertname, st.Len())
					met.RecordStoreRejected()
				}
				if ok {
					// #122 false positive guard. ConfidenceScore 가 threshold 미만 이면 metrics
					// emit 을 skip 하고 skipped_total counter 만 증가 한다. store 는 그대로 유지
					// 되어 운영자가 /rca?alert=<name> 으로 진단 직접 조회 가능 하다.
					if summary.ConfidenceScore < confidenceThreshold {
						log.Printf("rca: skip emit alert=%s confidence=%.3f below threshold %.3f", alertname, summary.ConfidenceScore, confidenceThreshold)
						met.RecordSkipped(alertname, "below_threshold")
					} else {
						// mapping 등록 alert 만 metrics 에 emit 해 alert_name 라벨 카디널리티
						// 폐쇄성을 보장 한다. 미등록 alert 의 진단 흐름은 /rca endpoint 의 store
						// entry 로 유지.
						met.Record(summary)
					}
				}
				mu.Lock()
				processed++
				mu.Unlock()
			}(a)
		}
		wg.Wait()
		if canceled > 0 {
			met.RecordWebhookDropped("canceled", canceled)
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
			// #447 표준 ErrorBody. 종전 http.Error 는 위에서 세팅한 application/json 을
			// text/plain 으로 덮어써 JSON 소비자의 파싱 실패를 유발했다.
			apicommon.WriteError(w, http.StatusNotFound, "unknown_alert", "alert 의 summary 가 없습니다: "+alertname)
			return
		}
		if err := json.NewEncoder(w).Encode(entry); err != nil {
			log.Printf("rca entry encode: %v", err)
		}
	})
}
