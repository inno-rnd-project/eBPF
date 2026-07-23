package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// SynthesisHandler 는 #178 설계의 합성 (synthesis) API endpoint 의존성을 모은다. Prometheus instant
// query 로 health / pressure recording rule 의 현재 값을 조회해 "헬스 + 압박 위치" 를 한 응답으로
// 합성한다. noisy-neighbor / impact-path snapshot 을 재사용하는 /events 는 후속에서 source 를 추가한다.
type SynthesisHandler struct {
	querier   correlation.InstantQuerier
	neighbors SnapshotSource
	crossNode CrossNodeSnapshotSource
	// agentClient 는 #281 gpu-processes 프록시의 노드 agent 호출용 client 다. 생성자가 5s timeout
	// 기본값으로 채우고, 테스트는 필드를 직접 교체한다.
	agentClient *http.Client
}

// NewSynthesisHandler 는 InstantQuerier 와 noisy-neighbor SnapshotSource, cross-node interference
// snapshot source 를 주입받아 합성 handler 를 만든다. querier 가 nil 이면 health/pressure/node/topology
// 가 데이터 부재 (unknown) 응답을, neighbors 가 nil 이면 events 가 anomaly 만, crossNode 가 nil 이면
// topology 가 노드 엣지 없이 graceful 하게 응답한다.
func NewSynthesisHandler(querier correlation.InstantQuerier, neighbors SnapshotSource, crossNode CrossNodeSnapshotSource) *SynthesisHandler {
	return &SynthesisHandler{
		querier:     querier,
		neighbors:   neighbors,
		crossNode:   crossNode,
		agentClient: &http.Client{Timeout: 5 * time.Second},
	}
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
	mux.Handle("/api/v1/pressure", apicommon.Chain(
		http.HandlerFunc(h.GetPressure),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// /api/v1/node/{node} 는 경로 끝 segment 가 node 라 prefix 매칭 후 핸들러에서 파싱한다. 기존
	// 라우트와 동일하게 CORSMiddleware 가 OPTIONS preflight 를 처리하도록 method 패턴은 쓰지 않는다.
	// #308 의 {node}/resources 하위 경로는 세그먼트 수 기반 디스패처로 분기한다.
	mux.Handle("/api/v1/node/", apicommon.Chain(
		http.HandlerFunc(h.nodeSubroute),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// #307 pod 단위 종합 상세. {namespace}/{pod} 두 segment 를 prefix 매칭 후 핸들러에서 파싱한다.
	mux.Handle("/api/v1/pod/", apicommon.Chain(
		http.HandlerFunc(h.GetPodDetail),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/events", apicommon.Chain(
		http.HandlerFunc(h.GetEvents),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// #249 랜딩 대시보드 요약. 기존 판정 (alert 매칭, 압박 임계, weakest) 의 합성이라 의존성이 같다.
	mux.Handle("/api/v1/overview", apicommon.Chain(
		http.HandlerFunc(h.GetOverview),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// #249 랜딩 노드 그리드. 노드별 pod 상태 맵을 서버 판정으로 내장한다.
	mux.Handle("/api/v1/node-map", apicommon.Chain(
		http.HandlerFunc(h.GetNodeMap),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// #258 노드 GPU RCA 합성. scope=node gpu-idle 과 noisy-neighbor/cross-node snapshot 을 합성한다.
	mux.Handle("/api/v1/gpu-rca", apicommon.Chain(
		http.HandlerFunc(h.GetNodeGpuRca),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// #265 노드 raw 사용률 프록시. cadvisor / gpuobs 원시 게이지를 노드별로 얇게 노출한다.
	mux.Handle("/api/v1/node-vitals", apicommon.Chain(
		http.HandlerFunc(h.GetNodeVitals),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// #266 노드별 관측 에이전트 self-health. 알림 규칙과 동일 임계로 healthy/degraded 를 판정한다.
	mux.Handle("/api/v1/agents", apicommon.Chain(
		http.HandlerFunc(h.GetAgents),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/gpu-idle", apicommon.Chain(
		http.HandlerFunc(h.GetGpuIdle),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/gpu-status", apicommon.Chain(
		http.HandlerFunc(h.GetGpuStatus),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	// #281 노드 GPU 실행 프로세스 프록시. gpuobs agent 로컬 스냅샷을 단일 진입점으로 중계한다.
	mux.Handle("/api/v1/gpu-processes", apicommon.Chain(
		http.HandlerFunc(h.GetGpuProcesses),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/bandwidth", apicommon.Chain(
		http.HandlerFunc(h.GetBandwidth),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/nodes", apicommon.Chain(
		http.HandlerFunc(h.GetNodes),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/pods", apicommon.Chain(
		http.HandlerFunc(h.GetPods),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/latency-breakdown", apicommon.Chain(
		http.HandlerFunc(h.GetLatencyBreakdown),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/drops", apicommon.Chain(
		http.HandlerFunc(h.GetDrops),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/topology", apicommon.Chain(
		http.HandlerFunc(h.GetTopology),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/flows", apicommon.Chain(
		http.HandlerFunc(h.GetFlows),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/memory", apicommon.Chain(
		http.HandlerFunc(h.GetMemory),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// causeLinks 는 #237 의 이벤트 drill-down 링크 셋을 만든다. detail (이벤트 종류별 기본 상세) 에
// dimension 별 원인 축 API 링크를 더하고, 모든 링크에 이벤트 관찰 시점의 at 파라미터를 포함해
// 프론트엔드가 나중에 클릭해도 사건 시점 상태로 바로 이어지게 한다 (#235 의 시점 지정 조회와 결합).
// cpu 는 detail 의 pressure drill-down 이 원인 축을 겸하므로 추가 링크가 없다.
func causeLinks(dimension, at, detail string) map[string]string {
	links := map[string]string{"detail": withAt(detail, at)}
	switch dimension {
	case "gpu":
		links["gpu_idle"] = withAt("/api/v1/gpu-idle", at)
		links["gpu_status"] = withAt("/api/v1/gpu-status", at)
	case "network":
		links["drops"] = withAt("/api/v1/drops", at)
		links["latency_breakdown"] = withAt("/api/v1/latency-breakdown", at)
	case "memory":
		links["memory"] = withAt("/api/v1/memory", at)
	}
	return links
}

// withAt 은 링크에 at 파라미터를 기존 쿼리 유무에 맞는 구분자로 덧붙인다.
func withAt(link, at string) string {
	sep := "?"
	if strings.Contains(link, "?") {
		sep = "&"
	}
	return link + sep + "at=" + at
}

// severityRank 는 severity 라벨을 정렬용 정수로 환산한다 (높을수록 심각). events 정렬과 min_severity
// 필터에 쓴다.
func severityRank(sev string) int {
	switch sev {
	case "high":
		return 3
	case "elevated":
		return 2
	case "low":
		return 1
	default:
		return 0
	}
}

// lookupDimension 은 차원 이름으로 synthDimension 을 찾는다.
func lookupDimension(name string) (synthDimension, bool) {
	for _, d := range synthDimensions {
		if d.name == name {
			return d, true
		}
	}
	return synthDimension{}, false
}

// pressureStatusLabel 은 pressure severity 를 health status 어휘 (ok/warn/degraded) 로 환산해 node
// 응답이 health 와 동일 status 어휘를 쓰게 한다.
func pressureStatusLabel(p float64) string {
	switch correlation.PressureSeverity(p) {
	case "high":
		return "degraded"
	case "elevated":
		return "warn"
	case "low":
		return "ok"
	default:
		return "unknown"
	}
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
	GeneratedAt string `json:"generated_at"`
	Window      string `json:"window"`
	// ClusterHealth 는 차원 health 의 최솟값 (가장 약한 고리 원칙, #248) 이다. 프론트 랜딩 카드의
	// 단일 % 표기가 본 필드를 그대로 쓴다. health 를 아는 차원이 하나도 없으면 생략된다.
	ClusterHealth *float64 `json:"cluster_health,omitempty"`
	// Weakest 는 ClusterHealth 를 만든 차원이다. 동률은 차원 이름 사전순 첫 항목으로 결정적이다.
	Weakest          *WeakestSignal             `json:"weakest,omitempty"`
	Dimensions       map[string]DimensionHealth `json:"dimensions"`
	DominantPressure *DominantPressure          `json:"dominant_pressure"`
	Anomalies        []Anomaly                  `json:"anomalies"`
	Summary          string                     `json:"summary"`
}

// WeakestSignal 은 health 가 가장 낮은 차원의 요약이다.
type WeakestSignal struct {
	Dimension string  `json:"dimension"`
	Health    float64 `json:"health"`
	Status    string  `json:"status"`
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
// @Description  4 자원 차원(cpu/gpu/memory/network)의 health 점수와 status, 압박이 집중된 node/pod(hotspot), 전체 dominant 압박 지점, z-score 이상, 한 줄 요약을 한 응답으로 돌려준다. cluster_health 는 차원 health 의 최솟값(가장 약한 고리)이고 weakest 가 그 차원을 가리켜 랜딩 카드의 단일 % 표기에 쓰인다. 데이터 부재는 null + status=unknown 으로 graceful 처리한다.
// @Tags         meta
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

	// 차원별 health·hotspot·z-score 조회를 goroutine 으로 병렬화해 한 요청당 순차 HTTP 누적을 막는다.
	// 각 goroutine 은 사전 할당 슬라이스의 자기 인덱스에만 기록하므로 공유 map 동시 쓰기나 mutex 가 없다.
	type dimResult struct {
		name    string
		dh      DimensionHealth
		hotspot *Hotspot
		anomaly *Anomaly
	}
	results := make([]dimResult, len(synthDimensions))
	var wg sync.WaitGroup
	for i := range synthDimensions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := synthDimensions[i]
			res := dimResult{name: d.name, dh: DimensionHealth{Status: "unknown"}}
			if h.querier != nil {
				if s, err := h.querier.Query(ctx, d.healthMetric); err == nil && len(s) > 0 && !math.IsNaN(s[0].Value) {
					v := s[0].Value
					res.dh.Health = &v
					res.dh.Status = correlation.HealthStatus(v)
				}
				res.hotspot = h.hotspot(ctx, d)
				res.dh.Hotspot = res.hotspot
				if s, err := h.querier.Query(ctx, d.zscoreMetric); err == nil && len(s) > 0 && !math.IsNaN(s[0].Value) {
					z := s[0].Value
					if sev := correlation.ZScoreSeverity(z); sev != "none" {
						res.anomaly = &Anomaly{Dimension: d.name, ZScore: z, Window: "5m", Severity: sev}
					}
				}
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	var dominant *DominantPressure
	for _, res := range results {
		resp.Dimensions[res.name] = res.dh
		if res.hotspot != nil && (dominant == nil || res.hotspot.Pressure > dominant.Pressure) {
			dominant = &DominantPressure{Dimension: res.name, Node: res.hotspot.Node, Pod: res.hotspot.TopPod, Pressure: res.hotspot.Pressure}
		}
		if res.anomaly != nil {
			resp.Anomalies = append(resp.Anomalies, *res.anomaly)
		}
		// #248 가장 약한 고리. results 는 synthDimensions 선언 순서 (사전순) 라 동률 시 첫 항목이
		// 결정적으로 채택된다.
		if res.dh.Health != nil && (resp.Weakest == nil || *res.dh.Health < resp.Weakest.Health) {
			resp.Weakest = &WeakestSignal{Dimension: res.name, Health: *res.dh.Health, Status: res.dh.Status}
			resp.ClusterHealth = res.dh.Health
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
	if err != nil || len(s) == 0 || math.IsNaN(s[0].Value) {
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

// PressureResponse 는 GET /api/v1/pressure 의 typed 응답이다. 한 차원의 node 또는 pod 를 pressure
// 내림차순으로 랭킹한다.
type PressureResponse struct {
	GeneratedAt string          `json:"generated_at"`
	Window      string          `json:"window"`
	Dimension   string          `json:"dimension"`
	Scope       string          `json:"scope"`
	Ranking     []PressureEntry `json:"ranking"`
	Summary     string          `json:"summary"`
}

// PressureEntry 는 pressure 랭킹의 한 항목이다. scope=pod 일 때만 Pod 가 채워진다.
type PressureEntry struct {
	Rank     int     `json:"rank"`
	Node     string  `json:"node"`
	Pod      string  `json:"pod,omitempty"`
	Pressure float64 `json:"pressure"`
	Severity string  `json:"severity"`
}

// GetPressure godoc
// @Summary      차원별 압박 drill-down 랭킹
// @Description  한 자원 차원의 node 또는 pod 를 pressure score 내림차순으로 랭킹해, /health 의 hotspot 에서 한 단계 더 파고든다.
// @Tags         interference
// @Produce      json
// @Param        dimension  query  string  true   "압박 차원 (cpu/gpu/memory/network)"
// @Param        scope      query  string  false  "랭킹 입도 (node/pod, 기본 node)"
// @Param        node       query  string  false  "단일 노드 필터 (DNS-1123 형식, 생략 시 전체)"
// @Param        limit      query  int     false  "상위 N (1-100, 기본 10)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  PressureResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/pressure [get]
func (h *SynthesisHandler) GetPressure(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d, ok := lookupDimension(strings.ToLower(strings.TrimSpace(q.Get("dimension"))))
	if !ok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_dimension", "dimension 은 cpu / gpu / memory / network 중 하나여야 합니다")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(q.Get("scope")))
	if scope == "" {
		scope = "node"
	}
	if scope != "node" && scope != "pod" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_scope", "scope 는 node 또는 pod 여야 합니다")
		return
	}
	limit := 10
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 100 {
		limit = 100
	}
	node, err := parseNodeParam(strings.TrimSpace(q.Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}

	metric := d.nodePressure
	if scope == "pod" {
		metric = d.podPressure
	}
	// #263 node 필터. pressure recording rule 은 node 라벨을 보유하므로 검증된 node 로 exact 매처를
	// metric 뒤에 붙인다. node 미지정이면 selector 가 빈 문자열이라 기존 전체 동작을 유지한다.
	metric += promSelector(nodeMatcher(node))

	evalCtx, evalAt, ok2 := applyAtParam(w, r, r.Context())
	if !ok2 {
		return
	}

	resp := PressureResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Window:      "5m",
		Dimension:   d.name,
		Scope:       scope,
		Ranking:     []PressureEntry{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
		defer cancel()
		if samples, err := h.querier.Query(ctx, fmt.Sprintf("topk(%d, %s)", limit, metric)); err == nil {
			// NaN 은 JSON 직렬화 실패와 비일관 정렬을 유발하므로 랭킹 전에 걸러낸다.
			valid := samples[:0]
			for _, s := range samples {
				if !math.IsNaN(s.Value) {
					valid = append(valid, s)
				}
			}
			samples = valid
			sort.Slice(samples, func(i, j int) bool {
				if samples[i].Value != samples[j].Value {
					return samples[i].Value > samples[j].Value
				}
				return podLabel(samples[i].Labels)+samples[i].Labels["node"] < podLabel(samples[j].Labels)+samples[j].Labels["node"]
			})
			for i, s := range samples {
				e := PressureEntry{Rank: i + 1, Node: s.Labels["node"], Pressure: s.Value, Severity: correlation.PressureSeverity(s.Value)}
				if scope == "pod" {
					e.Pod = podLabel(s.Labels)
				}
				resp.Ranking = append(resp.Ranking, e)
			}
		}
	}
	resp.Summary = buildPressureSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildPressureSummary 는 랭킹 상위와 elevated 이상 건수를 한 줄로 요약한다.
func buildPressureSummary(r PressureResponse) string {
	if len(r.Ranking) == 0 {
		return fmt.Sprintf("%s 압박 데이터가 없습니다.", r.Dimension)
	}
	elevated := 0
	for _, e := range r.Ranking {
		if e.Severity == "high" || e.Severity == "elevated" {
			elevated++
		}
	}
	top := r.Ranking[0]
	who := top.Node
	if top.Pod != "" {
		who = top.Pod
	}
	return fmt.Sprintf("%s 압박 %s 기준 상위 %s(%.2f, %s). elevated 이상 %d건.", r.Dimension, r.Scope, who, top.Pressure, top.Severity, elevated)
}

// NodeResponse 는 GET /api/v1/node/{node} 의 typed 응답이다. 노드 1대의 차원별 압박과 종합, dominant
// 차원, top 압박 pod 를 모은다. anomaly 는 cluster 단위 z-score 라 node 로 scope 되지 않아 본 응답에는
// 싣지 않고 /health 또는 /events 에서 본다.
type NodeResponse struct {
	GeneratedAt string             `json:"generated_at"`
	Window      string             `json:"window"`
	Node        string             `json:"node"`
	Pressure    map[string]float64 `json:"pressure"`
	// Health 는 차원별 health score (0-1, #264) 다. node:*_health_score:5m 룰 기반이며 cluster
	// health 와 동일 산식의 노드 차원 판이라 GPU 와 비GPU 노드가 같은 해석을 공유한다.
	Health  map[string]float64 `json:"health"`
	Overall *float64           `json:"overall"`
	Status  string             `json:"status"`
	// StatusBasis 는 status 등급을 결정한 신호다 (#324, pressure / health / alert). 등급 동률이면
	// pressure, health, alert 순으로 귀속하며 status 가 unknown (신호 전부 부재) 이면 생략된다.
	StatusBasis string `json:"status_basis,omitempty"`
	// Confidence 는 dominant 차원 판정 신뢰도 (0-1, #264) 다. 압박 top1 과 top2 차원의 격차로,
	// gpu-rca 의 신뢰도와 동일 축이라 한 차원이 지배적일수록 1 에 가깝다.
	Confidence        float64           `json:"confidence"`
	DominantDimension string            `json:"dominant_dimension"`
	TopPods           []NodePodPressure `json:"top_pods"`
	Summary           string            `json:"summary"`
}

// NodePodPressure 는 노드 위 한 pod 의 차원별 압박이다.
type NodePodPressure struct {
	Pod       string  `json:"pod"`
	Dimension string  `json:"dimension"`
	Pressure  float64 `json:"pressure"`
}

// GetNode godoc
// @Summary      노드 1대 전체 압박 상황
// @Description  노드의 4 차원 pressure 와 health, 종합(overall), dominant 차원과 신뢰도, 그 노드 위 top 압박 pod 를 모아 한 노드의 상태를 한 응답으로 돌려준다. health 는 node:*_health_score:5m 룰(cluster health 의 노드 차원 판)이고, 신뢰도는 압박 top1 과 top2 차원 격차라 gpu-rca 와 동일 축이다. status 는 dominant pressure 등급과 차원별 health 최솟값 등급과 이 노드를 가리키는 firing alert(warn 고정)의 worst-of 합성(#324)이며, status_basis 가 결정 신호(pressure/health/alert)를 담는다. 등급 어휘 ok/warn/degraded 는 overview 와 node-map 의 healthy/warning 과 같은 입력 신호(pressure, firing alert)를 공유하고, down(ready 기반) 판정은 node-map 소관이다.
// @Tags         interference
// @Produce      json
// @Param        node  path  string  true  "노드 이름"
// @Success      200  {object}  NodeResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/node/{node} [get]
func (h *SynthesisHandler) GetNode(w http.ResponseWriter, r *http.Request) {
	node := strings.TrimPrefix(r.URL.Path, "/api/v1/node/")
	if node == "" || strings.Contains(node, "/") {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", "경로는 /api/v1/node/{node} 형식이어야 합니다")
		return
	}

	resp := NodeResponse{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Window:      "5m",
		Node:        node,
		Pressure:    map[string]float64{},
		Health:      map[string]float64{},
		Status:      "unknown",
		TopPods:     []NodePodPressure{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		domDim, domVal := "", math.Inf(-1)
		for _, d := range synthDimensions {
			if s, err := h.querier.Query(ctx, fmt.Sprintf("%s{node=%q}", d.nodePressure, node)); err == nil && len(s) > 0 && !math.IsNaN(s[0].Value) {
				resp.Pressure[d.name] = s[0].Value
				if s[0].Value > domVal {
					domVal = s[0].Value
					domDim = d.name
				}
			}
			// #264 노드 차원 health. node:{dim}_health_score:5m 룰을 노드로 좁혀 읽는다.
			if s, err := h.querier.Query(ctx, fmt.Sprintf("node:%s_health_score:5m{node=%q}", d.name, node)); err == nil && len(s) > 0 && !math.IsNaN(s[0].Value) {
				resp.Health[d.name] = s[0].Value
			}
			if ps, err := h.querier.Query(ctx, fmt.Sprintf("topk(3, %s{node=%q})", d.podPressure, node)); err == nil {
				for _, p := range ps {
					if math.IsNaN(p.Value) {
						continue
					}
					if pod := podLabel(p.Labels); pod != "" {
						resp.TopPods = append(resp.TopPods, NodePodPressure{Pod: pod, Dimension: d.name, Pressure: p.Value})
					}
				}
			}
		}
		resp.Confidence = pressureConfidence(resp.Pressure)
		if s, err := h.querier.Query(ctx, fmt.Sprintf("node:pressure_score:5m{node=%q}", node)); err == nil && len(s) > 0 && !math.IsNaN(s[0].Value) {
			v := s[0].Value
			resp.Overall = &v
		}
		resp.DominantDimension = domDim

		// #324 이 노드를 가리키는 firing alert. overview / node-map 의 alertedNodes 와 동일하게
		// ALERTS 의 node 라벨로 매칭한다.
		alertFiring := false
		if s, err := h.querier.Query(ctx, fmt.Sprintf(`ALERTS{alertstate="firing",node=%q}`, node)); err == nil && len(s) > 0 {
			alertFiring = true
		}

		// status 는 dominant pressure 등급, 차원별 health 최솟값 등급, firing alert 의 worst-of
		// 합성이다 (#324). dominant pressure 만으로는 pressure 계열 밖의 이상 (예: GPU throttle
		// alert firing + health 0.0) 이 가려져 node-map 의 warning 판정과 모순된다. overall
		// (node:pressure_score) 은 차원을 블렌딩해 단일 차원 hotspot 을 희석하므로 정보 필드로만
		// 노출하고 status 에는 쓰지 않는다 (예: memory 0.97 이어도 overall 0.24 면 ok 로 가려짐).
		resp.Status, resp.StatusBasis = composeNodeStatus(domDim, domVal, resp.Health, alertFiring)

		sort.Slice(resp.TopPods, func(i, j int) bool {
			if resp.TopPods[i].Pressure != resp.TopPods[j].Pressure {
				return resp.TopPods[i].Pressure > resp.TopPods[j].Pressure
			}
			return resp.TopPods[i].Pod < resp.TopPods[j].Pod
		})
		if len(resp.TopPods) > 5 {
			resp.TopPods = resp.TopPods[:5]
		}
	}

	resp.Summary = buildNodeSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// nodeStatusRank 는 node 상세 status 어휘 (ok/warn/degraded/unknown) 의 심각도 순위다. worst-of
// 합성 (#324) 의 등급 비교에 쓴다.
func nodeStatusRank(s string) int {
	switch s {
	case "degraded":
		return 3
	case "warn":
		return 2
	case "ok":
		return 1
	default:
		return 0
	}
}

// composeNodeStatus 는 #324 의 worst-of 합성이다. dominant pressure 등급, 차원별 health 최솟값
// 등급 (HealthStatus 매핑 재사용), 이 노드를 가리키는 firing alert 셋 중 가장 나쁜 등급을 채택하고
// 결정 신호를 basis 로 돌려준다. alert 는 nodeStatus (overview / node-map) 의 warning 규약을 따라
// severity 세분화 없이 warn 고정이다. 등급 동률이면 pressure, health, alert 순으로 귀속하며, 세
// 신호가 전부 부재면 unknown 과 빈 basis 를 돌려준다.
func composeNodeStatus(domDim string, domVal float64, health map[string]float64, alertFiring bool) (string, string) {
	status, basis := "unknown", ""
	consider := func(s, b string) {
		if nodeStatusRank(s) > nodeStatusRank(status) {
			status, basis = s, b
		}
	}
	if domDim != "" {
		consider(pressureStatusLabel(domVal), "pressure")
	}
	if len(health) > 0 {
		minHealth := math.Inf(1)
		for _, v := range health {
			if v < minHealth {
				minHealth = v
			}
		}
		consider(correlation.HealthStatus(minHealth), "health")
	}
	if alertFiring {
		consider("warn", "alert")
	}
	return status, basis
}

// pressureConfidence 는 dominant 차원 판정 신뢰도 (#264) 다. 차원 pressure 를 내림차순으로 봤을 때
// top1 과 top2 의 격차 (margin) 로, 한 차원이 지배적일수록 1 에 가깝고 두 차원이 백중이면 0 에
// 가깝다. gpu-rca 의 dominantConfidence 와 동일 축이다. 차원이 하나뿐이면 그 값이 그대로 신뢰도다.
func pressureConfidence(pressure map[string]float64) float64 {
	vals := make([]float64, 0, len(pressure))
	for _, v := range pressure {
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
	margin := vals[0]
	if len(vals) >= 2 {
		margin = vals[0] - vals[1]
	}
	if margin < 0 {
		margin = 0
	}
	if margin > 1 {
		margin = 1
	}
	return margin
}

// buildNodeSummary 는 dominant 차원과 status, 신뢰도, 주 압박 pod 를 한 줄 narrative 로 요약한다.
// status 가 pressure 밖 신호 (health / alert) 로 결정됐으면 그 근거를 함께 적는다 (#324).
func buildNodeSummary(r NodeResponse) string {
	if r.DominantDimension == "" {
		if r.StatusBasis != "" {
			return fmt.Sprintf("%s 의 압박 데이터가 없습니다. status 는 %s 기준 %s.", r.Node, r.StatusBasis, r.Status)
		}
		return fmt.Sprintf("%s 의 압박 데이터가 없습니다.", r.Node)
	}
	seg := fmt.Sprintf("%s는 %s가 dominant(%.2f, %s, 신뢰도 %.2f)", r.Node, r.DominantDimension, r.Pressure[r.DominantDimension], r.Status, r.Confidence)
	if r.StatusBasis != "" && r.StatusBasis != "pressure" {
		seg += fmt.Sprintf(". status 는 %s 기준", r.StatusBasis)
	}
	if len(r.TopPods) > 0 {
		seg += fmt.Sprintf(". 주 압박 pod %s", r.TopPods[0].Pod)
	}
	return seg + "."
}

// EventsResponse 는 GET /api/v1/events 의 typed 응답이다. anomaly 와 noisy-neighbor 를 severity 정렬
// 사건 목록으로 묶는다.
type EventsResponse struct {
	GeneratedAt string  `json:"generated_at"`
	Window      string  `json:"window"`
	Events      []Event `json:"events"`
	Summary     string  `json:"summary"`
}

// Event 는 단일 분석 사건이다. kind 에 따라 일부 필드 (zscore / causal_strength) 가 채워진다.
type Event struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Severity       string            `json:"severity"`
	Dimension      string            `json:"dimension,omitempty"`
	ZScore         *float64          `json:"zscore,omitempty"`
	CausalStrength *float64          `json:"causal_strength,omitempty"`
	Explanation    string            `json:"explanation"`
	Links          map[string]string `json:"links,omitempty"`
}

// GetEvents godoc
// @Summary      이벤트 분석 (anomaly + noisy-neighbor 종합)
// @Description  z-score 이상과 noisy-neighbor 간섭을 공통 severity 로 묶어 정렬된 사건 목록과 자연어 설명, drill-down 링크로 돌려준다. links 는 이벤트 종류별 detail 에 dimension 별 원인 축 API (gpu 는 gpu-idle 과 gpu-status, network 는 drops 와 latency-breakdown, memory 는 memory) 를 더하고, 모든 링크에 관찰 시점의 at 파라미터가 포함되어 사건 시점 조회로 바로 이어진다. min_severity 미만 사건은 제외한다.
// @Tags         interference
// @Produce      json
// @Param        min_severity  query  string  false  "최소 severity (low/elevated/high, 기본 elevated)"
// @Param        limit         query  int     false  "상위 N 사건 (1-50, 기본 20)"
// @Success      200  {object}  EventsResponse
// @Router       /api/v1/events [get]
func (h *SynthesisHandler) GetEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minRank := severityRank("elevated")
	if v := strings.ToLower(strings.TrimSpace(q.Get("min_severity"))); v != "" {
		if rk := severityRank(v); rk > 0 {
			minRank = rk
		}
	}
	limit := 20
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}

	observedAt := time.Now().UTC().Format(time.RFC3339)
	resp := EventsResponse{
		GeneratedAt: observedAt,
		Window:      "5m",
		Events:      []Event{},
	}

	events := make([]Event, 0, 16)
	// anomaly 사건: 차원별 z-score 이상.
	if h.querier != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		for _, d := range synthDimensions {
			if s, err := h.querier.Query(ctx, d.zscoreMetric); err == nil && len(s) > 0 {
				z := s[0].Value
				if sev := correlation.ZScoreSeverity(z); sev != "none" {
					zc := z
					events = append(events, Event{
						ID:          "anomaly:" + d.name,
						Kind:        "anomaly",
						Severity:    sev,
						Dimension:   d.name,
						ZScore:      &zc,
						Explanation: fmt.Sprintf("%s 압박이 최근 5m 기준선 대비 z=%.1f로 이탈.", d.name, z),
						Links:       causeLinks(d.name, observedAt, "/api/v1/pressure?dimension="+d.name+"&scope=pod"),
					})
				}
			}
		}
	}
	// noisy-neighbor 사건: causal_strength 를 severity 로 환산.
	if h.neighbors != nil {
		for _, n := range h.neighbors.Snapshot() {
			cs := n.CausalStrength
			sev := correlation.PressureSeverity(cs)
			events = append(events, Event{
				ID:             fmt.Sprintf("noisy-neighbor:%s→%s:%s", podID(n.Suspect), podID(n.Victim), n.Dimension),
				Kind:           "noisy_neighbor",
				Severity:       sev,
				Dimension:      string(n.Dimension),
				CausalStrength: &cs,
				Explanation:    neighborExplanation(n),
				Links:          causeLinks(string(n.Dimension), observedAt, "/api/v1/noisy-neighbor?victim_pod="+n.Victim.Pod),
			})
		}
	}

	// min_severity 필터 후 severity 내림차순, 동률은 강도 (zscore 절대값 / causal_strength) 내림차순.
	for _, e := range events {
		if severityRank(e.Severity) >= minRank {
			resp.Events = append(resp.Events, e)
		}
	}
	sort.SliceStable(resp.Events, func(i, j int) bool {
		ri, rj := severityRank(resp.Events[i].Severity), severityRank(resp.Events[j].Severity)
		if ri != rj {
			return ri > rj
		}
		return eventStrength(resp.Events[i]) > eventStrength(resp.Events[j])
	})
	if len(resp.Events) > limit {
		resp.Events = resp.Events[:limit]
	}

	resp.Summary = buildEventsSummary(resp.Events)
	apicommon.WriteJSON(w, resp)
}

// eventStrength 는 동률 severity 정렬용 강도 스칼라다. anomaly 는 z-score 절대값, noisy-neighbor 는
// causal_strength 를 쓴다.
func eventStrength(e Event) float64 {
	if e.ZScore != nil {
		return math.Abs(*e.ZScore)
	}
	if e.CausalStrength != nil {
		return *e.CausalStrength
	}
	return 0
}

// neighborExplanation 은 noisy-neighbor 한 건을 자연어 한 줄로 설명한다. Granger 유의성과 effect
// magnitude 가 있으면 덧붙인다.
func neighborExplanation(n correlation.NoisyNeighbor) string {
	seg := fmt.Sprintf("%s의 %s 압박이 %s %s와 상관 %.2f", podID(n.Suspect), n.Dimension, podID(n.Victim), n.VictimSignal, n.Score)
	if n.GrangerOK {
		seg += fmt.Sprintf(", Granger 유의(p=%.2g)", n.PValue)
	}
	if n.ImpactMagnitudeOK {
		seg += fmt.Sprintf(", 영향 크기 %.3g", n.ImpactMagnitude)
	}
	return seg + "."
}

// buildEventsSummary 는 사건 수와 최상위 severity 분포를 한 줄로 요약한다.
func buildEventsSummary(events []Event) string {
	if len(events) == 0 {
		return "기준 severity 이상의 활성 사건이 없습니다."
	}
	high := 0
	for _, e := range events {
		if e.Severity == "high" {
			high++
		}
	}
	return fmt.Sprintf("활성 사건 %d건 (high %d건). 최상위: %s", len(events), high, events[0].Explanation)
}

// podID 는 PodIdentity 를 "ns/pod" 로 합친다. pod 가 없으면 namespace 만, 둘 다 없으면 "_unknown".
func podID(p correlation.PodIdentity) string {
	switch {
	case p.Namespace != "" && p.Pod != "":
		return p.Namespace + "/" + p.Pod
	case p.Pod != "":
		return p.Pod
	case p.Namespace != "":
		return p.Namespace
	default:
		return "_unknown"
	}
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
