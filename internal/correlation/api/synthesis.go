package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// SynthesisHandler 는 #178 설계의 합성 (synthesis) API endpoint 의존성을 모은다. Prometheus instant
// query 로 health / pressure recording rule 의 현재 값을 조회해 "헬스 + 압박 위치" 를 한 응답으로
// 합성한다. noisy-neighbor / impact-path snapshot 을 재사용하는 /events 는 후속에서 source 를 추가한다.
type SynthesisHandler struct {
	querier correlation.InstantQuerier
}

// NewSynthesisHandler 는 InstantQuerier 를 주입받아 합성 handler 를 만든다. querier 가 nil 이면 모든
// endpoint 가 데이터 부재 (unknown) 응답을 graceful 하게 돌려준다.
func NewSynthesisHandler(querier correlation.InstantQuerier) *SynthesisHandler {
	return &SynthesisHandler{querier: querier}
}

// Register 는 합성 API 라우트를 mux 에 등록한다. 기존 correlation API 와 동일하게 Logging / Recover /
// CORS 미들웨어 체인을 적용해 frontend 대시보드의 cross-origin 호출을 허용한다.
func (h *SynthesisHandler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/health", apicommon.Chain(
		http.HandlerFunc(h.GetHealth),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// synthDimension 은 한 자원 차원의 health / pressure / anomaly recording rule 이름을 묶는다. 네 차원의
// rule 명을 한 자리에 두어 health / pressure / node / events 가 동일 출처를 공유한다.
type synthDimension struct {
	name         string
	healthMetric string
	nodePressure string
	podPressure  string
	zscoreMetric string
}

// synthDimensions 는 합성 API 가 다루는 4 자원 차원의 recording rule 매핑이다. gpu 의 pod 압박은
// node:gpu_pressure_score 가 집계하는 pod:host_compute_stall_score 를 그대로 쓴다.
var synthDimensions = []synthDimension{
	{"cpu", "cluster:cpu_health_score:5m", "node:cpu_pressure_score:5m", "pod:cpu_throttle_score:5m", "cluster:cpu_throttle_zscore:5m"},
	{"gpu", "cluster:gpu_health_score:5m", "node:gpu_pressure_score:5m", "pod:host_compute_stall_score:5m", "cluster:gpu_util_zscore:5m"},
	{"memory", "cluster:memory_health_score:5m", "node:memory_pressure_score:5m", "pod:memory_pressure_score:5m", "cluster:memory_pressure_zscore:5m"},
	{"network", "cluster:network_health_score:5m", "node:network_pressure_score:5m", "pod:network_pressure_score:5m", "cluster:network_drop_zscore:5m"},
}

// HealthResponse 는 GET /api/v1/health 의 typed 응답이다. swagger definition 과 frontend 타입의 단일
// 출처다.
type HealthResponse struct {
	GeneratedAt      string                     `json:"generated_at"`
	Window           string                     `json:"window"`
	Dimensions       map[string]DimensionHealth `json:"dimensions"`
	DominantPressure *DominantPressure          `json:"dominant_pressure"`
	Anomalies        []Anomaly                  `json:"anomalies"`
	Summary          string                     `json:"summary"`
}

// DimensionHealth 는 한 차원의 health 점수와 status, 압박 집중 hotspot 이다. Health 가 null 이면 데이터
// 부재 (status=unknown), Hotspot 이 nil 이면 압박 데이터가 없는 차원이다. NaN 은 JSON 직렬화가 불가해
// pointer + null 로 표현한다.
type DimensionHealth struct {
	Health  *float64 `json:"health"`
	Status  string   `json:"status"`
	Hotspot *Hotspot `json:"hotspot"`
}

// Hotspot 은 한 차원의 압박이 가장 집중된 node 와 그 위 top pod 다.
type Hotspot struct {
	Node     string  `json:"node"`
	Pressure float64 `json:"pressure"`
	TopPod   string  `json:"top_pod,omitempty"`
	Severity string  `json:"severity"`
}

// DominantPressure 는 클러스터 전체에서 가장 압박이 큰 단일 지점이다.
type DominantPressure struct {
	Dimension string  `json:"dimension"`
	Node      string  `json:"node"`
	Pod       string  `json:"pod,omitempty"`
	Pressure  float64 `json:"pressure"`
}

// Anomaly 는 z-score 기준선 이탈로 감지된 차원 이상이다.
type Anomaly struct {
	Dimension string  `json:"dimension"`
	ZScore    float64 `json:"zscore"`
	Window    string  `json:"window"`
	Severity  string  `json:"severity"`
}

// GetHealth godoc
// @Summary      클러스터 헬스 + 압박 위치 합성
// @Description  4 자원 차원(cpu/gpu/memory/network)의 health 점수와 status, 압박이 집중된 node/pod(hotspot), 전체 dominant 압박 지점, z-score 이상, 한 줄 요약을 한 응답으로 돌려준다. 데이터 부재는 null + status=unknown 으로 graceful 처리한다.
// @Tags         synthesis
// @Produce      json
// @Success      200  {object}  HealthResponse
// @Router       /api/v1/health [get]
func (h *SynthesisHandler) GetHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	resp := HealthResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      "5m",
		Dimensions:  make(map[string]DimensionHealth, len(synthDimensions)),
		Anomalies:   []Anomaly{},
	}

	var dominant *DominantPressure
	for _, d := range synthDimensions {
		dh := DimensionHealth{Status: "unknown"}
		if h.querier != nil {
			if s, err := h.querier.Query(ctx, d.healthMetric); err == nil && len(s) > 0 && !math.IsNaN(s[0].Value) {
				v := s[0].Value
				dh.Health = &v
				dh.Status = correlation.HealthStatus(v)
			}
			if hs := h.hotspot(ctx, d); hs != nil {
				dh.Hotspot = hs
				if dominant == nil || hs.Pressure > dominant.Pressure {
					dominant = &DominantPressure{Dimension: d.name, Node: hs.Node, Pod: hs.TopPod, Pressure: hs.Pressure}
				}
			}
		}
		resp.Dimensions[d.name] = dh
	}

	if h.querier != nil {
		for _, d := range synthDimensions {
			if s, err := h.querier.Query(ctx, d.zscoreMetric); err == nil && len(s) > 0 {
				z := s[0].Value
				if sev := correlation.ZScoreSeverity(z); sev != "none" {
					resp.Anomalies = append(resp.Anomalies, Anomaly{Dimension: d.name, ZScore: z, Window: "5m", Severity: sev})
				}
			}
		}
	}

	resp.DominantPressure = dominant
	resp.Summary = buildHealthSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// hotspot 은 한 차원의 압박 집중 node 와 그 위 top pod 를 instant query (topk 1) 로 구한다. 데이터가
// 없으면 nil 을 돌려준다.
func (h *SynthesisHandler) hotspot(ctx context.Context, d synthDimension) *Hotspot {
	s, err := h.querier.Query(ctx, "topk(1, "+d.nodePressure+")")
	if err != nil || len(s) == 0 {
		return nil
	}
	node := s[0].Labels["node"]
	press := s[0].Value
	hs := &Hotspot{Node: node, Pressure: press, Severity: correlation.PressureSeverity(press)}
	if node != "" {
		if ps, err := h.querier.Query(ctx, fmt.Sprintf("topk(1, %s{node=%q})", d.podPressure, node)); err == nil && len(ps) > 0 {
			hs.TopPod = podLabel(ps[0].Labels)
		}
	}
	return hs
}

// podLabel 은 pod 압박 시계열의 src_namespace / src_pod 를 "ns/pod" 로 합친다. src_pod 가 없으면 빈
// 문자열을 돌려준다.
func podLabel(labels map[string]string) string {
	pod := labels["src_pod"]
	if pod == "" {
		return ""
	}
	ns := labels["src_namespace"]
	if ns == "" {
		ns = "_unknown"
	}
	return ns + "/" + pod
}

// buildHealthSummary 는 가장 나쁜 차원과 dominant 압박, 이상 건수를 자연어 한 줄로 요약한다. 차원
// 순회는 synthDimensions 순서를 따라 결정적이다.
func buildHealthSummary(r HealthResponse) string {
	worst := ""
	worstHealth := math.Inf(1)
	for _, d := range synthDimensions {
		dh := r.Dimensions[d.name]
		if dh.Health == nil {
			continue
		}
		if *dh.Health < worstHealth {
			worstHealth = *dh.Health
			worst = d.name
		}
	}
	if worst == "" {
		return "관측 가능한 health 데이터가 없습니다."
	}
	wd := r.Dimensions[worst]
	seg := fmt.Sprintf("%s health %.2f(%s)", worst, *wd.Health, wd.Status)
	if wd.Hotspot != nil {
		seg += fmt.Sprintf(", 압박 집중 %s", wd.Hotspot.Node)
		if wd.Hotspot.TopPod != "" {
			seg += "(" + wd.Hotspot.TopPod + ")"
		}
	}
	parts := []string{seg}
	if len(r.Anomalies) > 0 {
		parts = append(parts, fmt.Sprintf("z-score 이상 %d건", len(r.Anomalies)))
	}
	return strings.Join(parts, ". ") + "."
}
