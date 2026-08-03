package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
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
	// AlertMinHold 는 firing alert 가 node status 에 반영되기 위한 최소 active 지속 시간이다 (#379).
	// 생성자가 defaultAlertStatusMinHold 로 채우고, correlation-exporter 가 ALERT_STATUS_MIN_HOLD env
	// (또는 -alert-status-min-hold flag) 로 클러스터별 튜닝을 주입한다 (FETCH_TIMEOUT 등과 동일 관행).
	AlertMinHold time.Duration
}

// NewSynthesisHandler 는 InstantQuerier 와 noisy-neighbor SnapshotSource, cross-node interference
// snapshot source 를 주입받아 합성 handler 를 만든다. querier 가 nil 이면 health/pressure/node/topology
// 가 데이터 부재 (unknown) 응답을, neighbors 가 nil 이면 events 가 anomaly 만, crossNode 가 nil 이면
// topology 가 노드 엣지 없이 graceful 하게 응답한다.
func NewSynthesisHandler(querier correlation.InstantQuerier, neighbors SnapshotSource, crossNode CrossNodeSnapshotSource) *SynthesisHandler {
	return &SynthesisHandler{
		querier:      querier,
		neighbors:    neighbors,
		crossNode:    crossNode,
		agentClient:  &http.Client{Timeout: 5 * time.Second},
		AlertMinHold: defaultAlertStatusMinHold,
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

// pressureSeverityFor 는 차원별 hotspot severity 를 low / elevated / high 어휘로 돌려준다. memory 의
// node pressure 는 node_exporter 실측 사용률 (0~1) 이라 일반 임계 (0.4/0.7) 로는 정상 상주 사용률이
// elevated 로 과민 판정되어 /node 의 nodePressureGrade (usage 임계 0.85/0.95, #340) 와 어긋난다.
// memory 만 usage 임계로 환산해 /health 와 /node 가 같은 노드에 동일 등급을 내게 하고, 문제 신호 기반
// score 인 cpu (throttle) · network (drop/retrans) · gpu (host_compute_stall) 는 일반 PressureSeverity
// 를 유지한다 (#359). 어휘 (low/elevated/high) 는 그대로라 Hotspot.Severity 스키마는 바뀌지 않는다.
func pressureSeverityFor(dim string, v float64) string {
	if dim != "memory" {
		return correlation.PressureSeverity(v)
	}
	switch {
	case math.IsNaN(v):
		return "unknown"
	case v >= correlation.NodeUsageDegradedThreshold:
		return "high"
	case v >= correlation.NodeUsageWarnThreshold:
		return "elevated"
	default:
		return "low"
	}
}

// pressureSeverityRank 는 hotspot severity 어휘의 심각도 순위다. dominant 비교에서 차원 간 raw pressure
// 척도 차이 (memory 사용률 vs cpu/gpu/network 문제 score) 를 넘어 severity 우선으로 정렬하는 데 쓴다.
func pressureSeverityRank(sev string) int {
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

// severityElevatedThreshold 는 차원이 elevated 로 진입하는 임계다. memory 는 실측 사용률이라 usage
// 임계 (0.85), 그 외는 문제 신호 기반이라 일반 임계 (0.4) 다.
func severityElevatedThreshold(dim string) float64 {
	if dim == "memory" {
		return correlation.NodeUsageWarnThreshold
	}
	return correlation.PressureElevatedThreshold
}

// severityProgress 는 pressure 를 차원의 elevated 임계로 정규화한 상대 위치다. severity 동률일 때
// 척도가 다른 차원 (memory usage vs cpu / network / gpu 문제 score) 을 raw 값으로 직접 비교하지 않고,
// 각자 자기 임계 대비 얼마나 진행했는지로 비교해 척도 중립적으로 dominant 를 고른다 (#359 리뷰).
func severityProgress(dim string, v float64) float64 {
	t := severityElevatedThreshold(dim)
	if t == 0 {
		return v
	}
	return v / t
}

// moreDominant 는 두 hotspot 을 severity 우선, 동급이면 severityProgress (자기 임계 대비 상대 위치) 로
// 비교한다. severity 환산 (#359) 이 tier 는 정합시켰지만 동률 구간의 raw pressure tie-break 에는 여전히
// 척도 불일치가 남아 (memory usage 0.87 이 cpu throttle 0.45 를 raw 값만으로 누름), tie-break 도
// 척도 중립 정규화로 바꿔 memory 사용률이 문제 신호 기반 차원을 부당히 선점하지 못하게 한다 (#359 리뷰).
func moreDominant(aDim string, a *Hotspot, bDim string, b *Hotspot) bool {
	ra, rb := pressureSeverityRank(a.Severity), pressureSeverityRank(b.Severity)
	if ra != rb {
		return ra > rb
	}
	return severityProgress(aDim, a.Pressure) > severityProgress(bDim, b.Pressure)
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
	// TopPodNamespace 와 TopPodName 은 top_pod 의 분리 필드다 (#383 additive). top_pod 는 ns/pod
	// 결합 id 표현으로 불변 유지하고 신규 소비는 분리 필드를 쓴다. namespace 미상이면 생략된다
	// (결합 표현의 _unknown sentinel 은 분리 필드에 오지 않는다).
	TopPodNamespace string `json:"top_pod_namespace,omitempty"`
	TopPodName      string `json:"top_pod_name,omitempty"`
	Severity        string `json:"severity"`
}

// DominantPressure 는 클러스터 전체에서 가장 압박이 큰 단일 지점이다.
type DominantPressure struct {
	Dimension string  `json:"dimension"`
	Node      string  `json:"node"`
	Pod       string  `json:"pod,omitempty"`
	Pressure  float64 `json:"pressure"`
	// Namespace 와 PodName 은 pod (ns/pod 결합 id 표현, 불변) 의 분리 필드다 (#383 additive).
	// namespace 미상이면 생략된다.
	Namespace string `json:"namespace,omitempty"`
	PodName   string `json:"pod_name,omitempty"`
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
// @Description  4 자원 차원(cpu/gpu/memory/network)의 health 점수와 status, 압박이 집중된 node/pod(hotspot), 전체 dominant 압박 지점, z-score 이상, 한 줄 요약을 한 응답으로 돌려준다. cluster_health 는 차원 health 의 최솟값(가장 약한 고리)이고 weakest 가 그 차원을 가리켜 랜딩 카드의 단일 % 표기에 쓰인다. hotspot 의 top_pod 와 dominant_pressure 의 pod 는 ns/pod 결합 id 표현으로 불변이며, 슬래시 파싱 없이 신원을 읽는 분리 필드(top_pod_namespace/top_pod_name, namespace/pod_name)가 additive 로 병기된다(#383, namespace 미상이면 생략). 데이터 부재는 null + status=unknown 으로 graceful 처리한다.
// @Tags         meta
// @Produce      json
// @Success      200  {object}  HealthResponse
// @Failure      500  {object}  apicommon.ErrorBody
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
	var domHotspot *Hotspot
	var domDim string
	for _, res := range results {
		resp.Dimensions[res.name] = res.dh
		// dominant 는 severity 우선, 동급이면 severityProgress (자기 임계 대비 상대 위치) 로 고른다
		// (#359). 차원 인지 severity 와 척도 중립 tie-break 로 raw 사용률 큰 memory 의 부당한 dominant
		// 선점을 tier·동률 양쪽에서 막는다.
		if res.hotspot != nil && (domHotspot == nil || moreDominant(res.name, res.hotspot, domDim, domHotspot)) {
			domHotspot = res.hotspot
			domDim = res.name
			dominant = &DominantPressure{Dimension: res.name, Node: res.hotspot.Node, Pod: res.hotspot.TopPod, Pressure: res.hotspot.Pressure, Namespace: res.hotspot.TopPodNamespace, PodName: res.hotspot.TopPodName}
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
	hs := &Hotspot{Node: node, Pressure: press, Severity: pressureSeverityFor(d.name, press)}
	if node != "" {
		if ps, err := h.querier.Query(ctx, fmt.Sprintf("topk(1, %s{node=%q})", d.podPressure, node)); err == nil && len(ps) > 0 {
			hs.TopPod = podLabel(ps[0].Labels)
			if ns, name := podFields(ps[0].Labels); name != "" {
				hs.TopPodNamespace, hs.TopPodName = ns, name
			}
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
	// Total 은 limit 적용 전 전체 랭킹 대상 수, Truncated 는 Total 이 limit 을 초과해 잘렸는지다
	// (#352). 클라이언트가 결과가 잘렸는지 판단할 수 있게 한다.
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Summary   string `json:"summary"`
}

// PressureEntry 는 pressure 랭킹의 한 항목이다. scope=pod 일 때만 Pod 계열이 채워진다.
type PressureEntry struct {
	Rank int    `json:"rank"`
	Node string `json:"node"`
	Pod  string `json:"pod,omitempty"`
	// Namespace 와 PodName 은 pod (ns/pod 결합 id 표현, 불변) 의 분리 필드다 (#383 additive).
	// 프론트가 슬래시 파싱 없이 신원을 읽는 표준 경로이며 namespace 미상이면 생략된다.
	Namespace string  `json:"namespace,omitempty"`
	PodName   string  `json:"pod_name,omitempty"`
	Pressure  float64 `json:"pressure"`
	Severity  string  `json:"severity"`
}

// GetPressure godoc
// @Summary      차원별 압박 drill-down 랭킹
// @Description  한 자원 차원의 node 또는 pod 를 pressure score 내림차순으로 랭킹해, /health 의 hotspot 에서 한 단계 더 파고든다. scope=pod 의 pod 는 ns/pod 결합 id 표현으로 불변이며, 슬래시 파싱 없이 신원을 읽는 분리 필드 namespace 와 pod_name 이 additive 로 병기된다(#383, namespace 미상이면 생략).
// @Tags         interference
// @Produce      json
// @Param        dimension  query  string  true   "압박 차원 (cpu/gpu/memory/network)"
// @Param        scope      query  string  false  "랭킹 입도 (node/pod, 기본 node)"
// @Param        node       query  string  false  "단일 노드 필터 (DNS-1123 형식, 생략 시 전체)"
// @Param        limit      query  int     false  "상위 N (1-100, 기본 10)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  PressureResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
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
	limit, ok := apicommon.ParseLimit(r, 10, 100)
	if !ok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit 은 정수여야 합니다")
		return
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
		// #352 total 을 알기 위해 topk 대신 전체를 조회해 Go 에서 정렬·절단한다. pressure 시리즈는
		// node (scope=node) 또는 pod (scope=pod) 수로 상한돼 전체 조회 비용이 통제된다. 쿼리 실패는
		// 백엔드 장애라 500 query_failed 로 통일한다 (#352, 데이터 부재는 빈 결과 → 200).
		samples, err := h.querier.Query(ctx, metric)
		if err != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
			return
		}
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
		resp.Total = len(samples)
		if len(samples) > limit {
			samples = samples[:limit]
			resp.Truncated = true
		}
		for i, s := range samples {
			// scope=node memory 는 /health · /node 와 같은 node:memory_pressure_score (실측 사용률) 라
			// usage 임계로 환산해 정합시킨다 (#359 리뷰). scope=pod memory 는 pod:memory_pressure_score
			// (working_set 대비 limit 비율 = OOM 근접도) 라 usage 임계가 무의미해 일반 임계를 유지한다.
			sev := correlation.PressureSeverity(s.Value)
			if d.name == "memory" && scope == "node" {
				sev = pressureSeverityFor(d.name, s.Value)
			}
			e := PressureEntry{Rank: i + 1, Node: s.Labels["node"], Pressure: s.Value, Severity: sev}
			if scope == "pod" {
				e.Pod = podLabel(s.Labels)
				if ns, name := podFields(s.Labels); name != "" {
					e.Namespace, e.PodName = ns, name
				}
			}
			resp.Ranking = append(resp.Ranking, e)
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
	// StatusUnified 는 status 와 같은 판정을 노드 상태 단일 규약 어휘 (#381, healthy/warning/critical/
	// unknown) 로 노출하는 additive 필드다. 산정은 규약 어휘로 하고 기존 status (ok/warn/degraded/
	// unknown) 는 correlation.NodeDetailStatus 환원으로 불변 유지한다. down 은 ready 신호를 조회하지
	// 않아 방출하지 않는다 (node-map 소관). 신규 소비는 본 필드를 쓴다.
	StatusUnified string `json:"status_unified"`
	// StatusBasis 는 status 등급을 결정한 신호다 (#324, #325, pressure / usage / health / alert).
	// 등급 동률이면 pressure, usage, health, alert 순으로 귀속하며 status 가 unknown (신호 전부
	// 부재) 이면 생략된다.
	StatusBasis string `json:"status_basis,omitempty"`
	// StatusAlerts 는 이 노드에서 지속성 게이트 (#379, alertStatusMinHold) 를 통과한 firing alertname
	// 목록이다 (additive). status_basis=alert 이면 이들이 status 를 올린 근거라 pressure/usage 가 정상인데
	// warn 인 이유를 추적하게 한다. 통과한 alert 가 없으면 생략된다. status/status_basis 필드는 불변이며
	// 본 필드만 additive 로 더한다 (비목표: 출력 스키마 변경 없음, additive 는 미인지 소비자가 무시).
	StatusAlerts []string `json:"status_alerts,omitempty"`
	// Confidence 는 dominant 차원 판정 신뢰도 (0-1, #264) 다. 압박 top1 과 top2 차원의 격차로,
	// gpu-rca 의 신뢰도와 동일 축이라 한 차원이 지배적일수록 1 에 가깝다.
	Confidence        float64           `json:"confidence"`
	DominantDimension string            `json:"dominant_dimension"`
	TopPods           []NodePodPressure `json:"top_pods"`
	Summary           string            `json:"summary"`
}

// NodePodPressure 는 노드 위 한 pod 의 차원별 압박이다.
type NodePodPressure struct {
	Pod string `json:"pod"`
	// Namespace 와 PodName 은 pod (ns/pod 결합 id 표현, 불변) 의 분리 필드다 (#383 additive).
	// 프론트가 슬래시 파싱 없이 신원을 읽는 표준 경로이며 namespace 미상이면 생략된다.
	Namespace string  `json:"namespace,omitempty"`
	PodName   string  `json:"pod_name,omitempty"`
	Dimension string  `json:"dimension"`
	Pressure  float64 `json:"pressure"`
}

// GetNode godoc
// @Summary      노드 1대 전체 압박 상황
// @Description  노드의 4 차원 pressure 와 health, 종합(overall), dominant 차원과 신뢰도, 그 노드 위 top 압박 pod 를 모아 한 노드의 상태를 한 응답으로 돌려준다. health 는 node:*_health_score:5m 룰(cluster health 의 노드 차원 판)이고, 신뢰도는 압박 top1 과 top2 차원 격차라 gpu-rca 와 동일 축이다. status 는 차원별 pressure 등급(전 차원 worst, memory 는 node_exporter 실측 사용률이라 일반 임계 대신 usage 임계 0.85/0.95 로 환산해 health 불감대·usage 축 설계와 정합)과 노드 사용량(CPU/memory 점유율, allocatable 분모, 0.85 warn/0.95 degraded) 등급과 차원별 health 최솟값 등급과 이 노드를 가리키는 firing alert(severity critical 은 degraded, 그 외 warn) 등급의 worst-of 합성(#324, #325)이며, status_basis 가 결정 신호(pressure/usage/health/alert)를 담는다. GPU 사용률은 포화가 정상 활용일 수 있어 등급 입력에서 제외한다. status 는 종전 어휘(ok/warn/degraded/unknown)를 유지하고, 같은 판정을 노드 상태 단일 규약 어휘(#381, healthy/warning/critical/unknown)로 노출하는 status_unified 필드가 additive 로 함께 실린다(ok 는 healthy, warn 은 warning, degraded 는 critical 대응). down(ready 기반) 판정은 ready 신호를 조회하지 않아 방출하지 않으며 node-map 소관이다. top_pods 는 dominant 압박의 원인 후보이며 is_filtered=true 면 pod pressure 가 elevated 임계(0.4) 이상인 pod 만 남기고 dominant severity 가 low(정상)면 목록을 비워 낮은 수치 pod 가 원인으로 오인되지 않게 한다(#380). 미전달 기본은 전 pod 를 노출한다. top_pods 의 pod 는 ns/pod 결합 id 표현으로 불변이며, 슬래시 파싱 없이 신원을 읽는 분리 필드 namespace 와 pod_name 이 additive 로 병기된다(#383, namespace 미상이면 생략).
// @Tags         interference
// @Produce      json
// @Param        node  path  string  true  "노드 이름"
// @Param        is_filtered  query  bool  false  "top_pods 를 원인 후보 임계(pressure>=0.4, dominant severity low 면 비움)로 필터링 (기본 false)"
// @Success      200  {object}  NodeResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/node/{node} [get]
func (h *SynthesisHandler) GetNode(w http.ResponseWriter, r *http.Request) {
	node := strings.TrimPrefix(r.URL.Path, "/api/v1/node/")
	if node == "" || strings.Contains(node, "/") {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", "경로는 /api/v1/node/{node} 형식이어야 합니다")
		return
	}
	// #380 is_filtered=true 면 top_pods 를 원인 후보 임계로 거른다 (opt-in). 미전달 기본은 종전대로 전
	// pod 를 노출해 기존 소비자에 무영향이다 (additive 쿼리 파라미터, 응답 스키마 불변).
	isFiltered := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("is_filtered")), "true")

	resp := NodeResponse{
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Window:        "5m",
		Node:          node,
		Pressure:      map[string]float64{},
		Health:        map[string]float64{},
		Status:        "unknown",
		StatusUnified: correlation.NodeStatusUnknown,
		TopPods:       []NodePodPressure{},
	}

	if h.querier != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		// #404 17개 쿼리는 전부 상호 독립이라 단일 queryParallel 라운드로 실행한다. 종전 완전 순차
		// 실행은 쿼리당 300ms 면 5s 예산을 초과했고, 초과 시점 이후 쿼리가 조용히 버려져 응답 내용이
		// Prometheus 지연에 따라 요청마다 달라졌다. 모든 쿼리가 status worst-of 합성의 입력이라 어느
		// 하나의 실패도 응답을 왜곡하므로 전부 필수이며, 실패는 inventory.go 규약대로 500 query_failed
		// 다 (데이터 부재로 결과가 빈 것은 종전대로 graceful 생략). ALERTS_FOR_STATE 는 종전에 firing
		// 존재 시에만 2차 조회했으나 병렬화로 무조건 조회하고 결과만 조건부 사용한다 (동일 의미).
		nDims := len(synthDimensions)
		queries := make([]string, 0, nDims*3+5)
		for _, d := range synthDimensions {
			queries = append(queries, fmt.Sprintf("%s{node=%q}", d.nodePressure, node))
		}
		for _, d := range synthDimensions {
			// #264 노드 차원 health. node:{dim}_health_score:5m 룰을 노드로 좁혀 읽는다.
			queries = append(queries, fmt.Sprintf("node:%s_health_score:5m{node=%q}", d.name, node))
		}
		for _, d := range synthDimensions {
			queries = append(queries, fmt.Sprintf("topk(3, %s{node=%q})", d.podPressure, node))
		}
		overallIdx := len(queries)
		queries = append(queries, fmt.Sprintf("node:pressure_score:5m{node=%q}", node))
		alertsIdx := len(queries)
		queries = append(queries, fmt.Sprintf(`ALERTS{alertstate="firing",node=%q}`, node))
		alertAgeIdx := len(queries)
		queries = append(queries, fmt.Sprintf(`time() - ALERTS_FOR_STATE{node=%q}`, node))
		usageIdx := len(queries)
		// #325 노드 사용량 점유율. node-vitals (#313) 와 동일한 allocatable 분모 산식 (pod-level
		// cgroup 행 한정, 분모 max 집계) 을 비율 (0~1) 로 읽는다. GPU 사용률은 포화가 정상 활용일
		// 수 있어 등급 입력에서 제외한다 (GPU 이상은 health 의 gpu 차원이 담당).
		queries = append(queries,
			fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{node=%q,container="",pod!=""}[5m])) / max(kube_node_status_allocatable{node=%q, resource="cpu"})`, node, node),
			fmt.Sprintf(`sum(container_memory_working_set_bytes{node=%q,container="",pod!=""}) / max(kube_node_status_allocatable{node=%q, resource="memory"})`, node, node),
		)
		res, qerr := h.queryParallel(ctx, queries...)
		if qerr != nil {
			apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
			return
		}

		domDim, domVal := "", math.Inf(-1)
		for i, d := range synthDimensions {
			if s := res[i]; len(s) > 0 && !math.IsNaN(s[0].Value) {
				resp.Pressure[d.name] = s[0].Value
				if s[0].Value > domVal {
					domVal = s[0].Value
					domDim = d.name
				}
			}
			if s := res[nDims+i]; len(s) > 0 && !math.IsNaN(s[0].Value) {
				resp.Health[d.name] = s[0].Value
			}
			for _, p := range res[nDims*2+i] {
				if math.IsNaN(p.Value) {
					continue
				}
				if pod := podLabel(p.Labels); pod != "" {
					ns, name := podFields(p.Labels)
					resp.TopPods = append(resp.TopPods, NodePodPressure{Pod: pod, Namespace: ns, PodName: name, Dimension: d.name, Pressure: p.Value})
				}
			}
		}
		resp.Confidence = pressureConfidence(resp.Pressure)
		if s := res[overallIdx]; len(s) > 0 && !math.IsNaN(s[0].Value) {
			v := s[0].Value
			resp.Overall = &v
		}
		resp.DominantDimension = domDim

		// #324 이 노드를 가리키는 firing alert. overview / node-map 의 alertedNodes 와 동일하게 ALERTS 의
		// node 라벨로 매칭하고 severity critical 포함 여부로 등급을 나눈다 (#325). #379 순간 firing 이
		// 아니라 pending 진입 후 alertStatusMinHold 이상 지속된 alert 만 반영해 transient flapping 을
		// 억제한다. active-age 는 time() - ALERTS_FOR_STATE (pending 진입 시각) 로 재고, 값이 없으면
		// (Prometheus 재시작 등) 지속성 확인 불가라 보수적으로 반영해 실제 alert 를 놓치지 않는다. 반영된
		// alertname 은 status_basis=alert 의 근거로 StatusAlerts 에 노출한다.
		alertGrade := ""
		if firing := res[alertsIdx]; len(firing) > 0 {
			// active-age (초) = time() - ALERTS_FOR_STATE. time() 은 query eval 시각이라 별도 wall-clock
			// 읽기 없이 지속 시간을 얻고, 인스턴스 단위 join 은 alertSignature 로 맞춘다.
			ageBySig := map[string]float64{}
			for _, sm := range res[alertAgeIdx] {
				ageBySig[alertSignature(sm.Labels)] = sm.Value
			}
			names := map[string]bool{}
			for _, sm := range firing {
				if age, ok := ageBySig[alertSignature(sm.Labels)]; ok && age < h.AlertMinHold.Seconds() {
					continue // 지속성 미달 (transient) → status 반영 보류.
				}
				grade := correlation.NodeStatusWarning
				if sm.Labels["severity"] == "critical" {
					grade = correlation.NodeStatusCritical
				}
				if correlation.NodeStatusRank(grade) > correlation.NodeStatusRank(alertGrade) {
					alertGrade = grade
				}
				if n := sm.Labels["alertname"]; n != "" {
					names[n] = true
				}
			}
			for n := range names {
				resp.StatusAlerts = append(resp.StatusAlerts, n)
			}
			sort.Strings(resp.StatusAlerts)
		}

		usage := []float64{}
		for _, s := range res[usageIdx : usageIdx+2] {
			if len(s) > 0 && !math.IsNaN(s[0].Value) {
				usage = append(usage, s[0].Value)
			}
		}

		// status 는 차원별 pressure 등급, 노드 사용량 등급, 차원별 health 최솟값 등급, firing
		// alert 등급의 worst-of 합성이다 (#324, #325). dominant pressure 만으로는 pressure 계열
		// 밖의 이상 (GPU throttle alert firing + health 0.0, limit 없는 pod 의 CPU 포화) 이 가려져
		// node-map 의 warning 판정과 모순된다. overall (node:pressure_score) 은 차원을 블렌딩해
		// 단일 차원 hotspot 을 희석하므로 정보 필드로만 노출하고 status 에는 쓰지 않는다 (예:
		// memory 0.97 이어도 overall 0.24 면 ok 로 가려짐). 산정은 단일 규약 어휘 (#381) 로 하고
		// 기존 status 어휘는 환원 매핑으로 유지한다.
		resp.StatusUnified, resp.StatusBasis = composeNodeStatus(nodePressureGrade(resp.Pressure), usage, resp.Health, alertGrade)
		resp.Status = correlation.NodeDetailStatus(resp.StatusUnified)

		sort.Slice(resp.TopPods, func(i, j int) bool {
			if resp.TopPods[i].Pressure != resp.TopPods[j].Pressure {
				return resp.TopPods[i].Pressure > resp.TopPods[j].Pressure
			}
			return resp.TopPods[i].Pod < resp.TopPods[j].Pod
		})
		// #380 is_filtered=true 면 top_pods 를 원인 후보 임계로 거른다. top_pods 는 dominant 압박의 원인
		// 후보라, 임계 미만 pod 와 정상 노드의 낮은 수치 후보를 제외해 사용자가 낮은 값 pod 를 원인으로
		// 오인하지 않게 한다. dominant 차원 severity 가 low (정상) 면 원인 후보가 없어 목록을 비우고
		// (memory 는 node 사용률 축이라 usage 임계로, 나머지는 pressure 임계로 등급화해 status 산정과
		// 규약을 맞춘다), elevated 이상이면 pod pressure 가 PressureElevatedThreshold (0.4) 이상인 pod 만
		// 남긴다. 정렬 뒤에 거르므로 결정적 순서 (pressure 내림차순 + pod 사전순 tie-break) 가 보존된다.
		// pod pressure score 산식과 top_pods 필드 형태는 불변이며 표시 임계만 opt-in 으로 더한다 (비목표).
		if isFiltered {
			dominantLow := domDim == ""
			if domDim != "" {
				g := correlation.NodeStatusFromPressure(domVal)
				if domDim == "memory" {
					g = correlation.NodeStatusFromUsage(domVal)
				}
				dominantLow = g == correlation.NodeStatusHealthy
			}
			kept := resp.TopPods[:0]
			if !dominantLow {
				for _, p := range resp.TopPods {
					if p.Pressure >= correlation.PressureElevatedThreshold {
						kept = append(kept, p)
					}
				}
			}
			resp.TopPods = kept
		}
		if len(resp.TopPods) > 5 {
			resp.TopPods = resp.TopPods[:5]
		}
	}

	resp.Summary = buildNodeSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// defaultAlertStatusMinHold 는 SynthesisHandler.AlertMinHold 의 기본값이다 (#379). firing alert 가
// node status 에 반영되기 위한 최소 active 지속 시간으로, alert 는 자체 for (예 5m) 로 pending debounce
// 를 갖지만 for 경과 직후 짧게 firing 후 resolved 되는 에피소드가 status 를 warn↔ok 로 진동시켰다.
// active-age (time() - ALERTS_FOR_STATE) 가 이 값 미만이면 status 반영을 보류해 status 측 hysteresis
// 를 더한다. active-age 는 pending 을 포함하므로 순수 firing 지속은 age - 각 alert 의 for 이고, 여기서는
// transient 억제가 목적이라 active 기준으로 단순화한다 (alert 마다 for 를 조회하지 않는다). node status
// 는 dashboard rollup 이라 이 지연이 수용되며 alert 자체 발화는 그대로다 (발화 로직은 각 alert 이슈 소관).
const defaultAlertStatusMinHold = 10 * time.Minute

// alertSignature 는 ALERTS 와 ALERTS_FOR_STATE 를 인스턴스 단위로 join 하기 위한 라벨 서명이다. 두
// 메트릭은 __name__ 과 alertstate 를 빼면 동일 라벨셋이라 그 둘을 제외한 라벨을 정렬해 잇는다 (#379).
func alertSignature(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "__name__" || k == "alertstate" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(0x1f)
	}
	return b.String()
}

// composeNodeStatus 는 #324 와 #325 의 4입력 worst-of 합성이다. 차원별 pressure 등급
// (nodePressureGrade), 노드 사용량 (CPU / memory 점유율) 등급, 차원별 health 최솟값 등급, 이
// 노드를 가리키는 firing alert 등급 (severity critical 은 critical, 그 외 warning, #325) 중 가장
// 나쁜 등급을 채택하고 결정 신호를 basis 로 돌려준다. 등급 동률이면 pressure, usage, health,
// alert 순으로 귀속하며, 신호가 전부 부재면 unknown 과 빈 basis 를 돌려준다. 입력과 반환 어휘는
// 단일 규약 (#381, correlation.NodeStatus*) 이다.
func composeNodeStatus(pressureGrade string, usage []float64, health map[string]float64, alertGrade string) (string, string) {
	status, basis := correlation.NodeStatusUnknown, ""
	consider := func(s, b string) {
		if correlation.NodeStatusRank(s) > correlation.NodeStatusRank(status) {
			status, basis = s, b
		}
	}
	if pressureGrade != "" {
		consider(pressureGrade, "pressure")
	}
	for _, frac := range usage {
		consider(correlation.NodeStatusFromUsage(frac), "usage")
	}
	if len(health) > 0 {
		minHealth := math.Inf(1)
		for _, v := range health {
			if v < minHealth {
				minHealth = v
			}
		}
		consider(correlation.NodeStatusFromHealthScore(minHealth), "health")
	}
	if alertGrade != "" {
		consider(alertGrade, "alert")
	}
	return status, basis
}

// nodePressureGrade 는 차원별 pressure 를 단일 규약 어휘 (#381) 로 환산한 뒤 가장 나쁜 등급을
// 돌려준다. memory 의 node pressure 는 node_exporter 실측 사용률 (0~1) 이라 일반 임계 (0.4/0.7)
// 로는 정상 상주 사용률이 경고로 과민 판정되어 node-map (블렌딩 pressure 기준 healthy) 과
// 모순됐다. health 의 위험 구간 매핑 (0.80 이하 불감대, #286) 과 usage 축 임계 (#325) 의 설계와
// 정합하도록 memory 만 usage 임계 (0.85/0.95) 로 환산한다. cpu (throttle) 와 network
// (drop/retrans) 와 gpu (host_compute_stall) 는 문제 신호 기반 score 라 일반 임계가 타당하며,
// 실측 사용률인 memory 만 재척도 대상이다. memory 재척도로 dominant (최대값) 차원의 등급이 최악
// 등급과 어긋날 수 있어 dominant 한 차원이 아니라 전 차원을 등급화해 worst 를 취한다.
func nodePressureGrade(pressure map[string]float64) string {
	grade := ""
	for dim, v := range pressure {
		g := correlation.NodeStatusFromPressure(v)
		if dim == "memory" {
			g = correlation.NodeStatusFromUsage(v)
		}
		if correlation.NodeStatusRank(g) > correlation.NodeStatusRank(grade) {
			grade = g
		}
	}
	return grade
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

// statusAlertSuffix 는 status_basis=alert 일 때 status 를 올린 지속 alertname 을 괄호로 덧붙인다 (#379).
// pressure/usage 가 정상인데 warn 인 근거를 요약에서 바로 읽게 한다. alert 기준이 아니면 빈 문자열이다.
func statusAlertSuffix(r NodeResponse) string {
	if r.StatusBasis == "alert" && len(r.StatusAlerts) > 0 {
		return " (" + strings.Join(r.StatusAlerts, ", ") + ")"
	}
	return ""
}

// buildNodeSummary 는 dominant 차원과 status, 신뢰도, 주 압박 pod 를 한 줄 narrative 로 요약한다.
// status 가 pressure 밖 신호 (health / alert) 로 결정됐으면 그 근거를 함께 적는다 (#324).
func buildNodeSummary(r NodeResponse) string {
	if r.DominantDimension == "" {
		if r.StatusBasis != "" {
			return fmt.Sprintf("%s 의 압박 데이터가 없습니다. status 는 %s 기준 %s%s.", r.Node, r.StatusBasis, r.Status, statusAlertSuffix(r))
		}
		return fmt.Sprintf("%s 의 압박 데이터가 없습니다.", r.Node)
	}
	seg := fmt.Sprintf("%s는 %s가 dominant(%.2f, %s, 신뢰도 %.2f)", r.Node, r.DominantDimension, r.Pressure[r.DominantDimension], r.Status, r.Confidence)
	if r.StatusBasis != "" && r.StatusBasis != "pressure" {
		seg += fmt.Sprintf(". status 는 %s 기준%s", r.StatusBasis, statusAlertSuffix(r))
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
	// Total 은 limit 적용 전 (min_severity 필터 후) 전체 사건 수, Truncated 는 잘렸는지다 (#352).
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Summary   string `json:"summary"`
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
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/events [get]
func (h *SynthesisHandler) GetEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minRank := severityRank("elevated")
	if v := strings.ToLower(strings.TrimSpace(q.Get("min_severity"))); v != "" {
		if rk := severityRank(v); rk > 0 {
			minRank = rk
		}
	}
	limit, ok := apicommon.ParseLimit(r, 20, 50)
	if !ok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit 은 정수여야 합니다")
		return
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
			s, err := h.querier.Query(ctx, d.zscoreMetric)
			if err != nil {
				// #352 z-score 이상은 events 의 primary source 라 백엔드 장애 시 500 으로 통일한다.
				apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
				return
			}
			if len(s) > 0 {
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
	resp.Total = len(resp.Events)
	if len(resp.Events) > limit {
		resp.Events = resp.Events[:limit]
		resp.Truncated = true
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

// podFields 는 pod 압박 시계열의 src_namespace / src_pod 를 분리 필드 값으로 돌려준다 (#383).
// podLabel 의 결합 표현과 달리 namespace 부재 시 sentinel (_unknown) 없이 빈 값을 돌려줘 응답에서
// omitempty 로 생략된다.
func podFields(labels map[string]string) (namespace, name string) {
	return labels["src_namespace"], labels["src_pod"]
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
