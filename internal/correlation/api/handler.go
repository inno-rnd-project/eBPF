// Package api 는 이슈 #100 의 자체 dashboard 용 REST API layer 의 correlation-exporter 측
// 구현이다. 운영자 또는 자체 dashboard 가 Prometheus query 없이 noisy neighbor top-N 결과를
// JSON 으로 직접 조회 가능 하게 한다. 데이터 source 는 exporter.Collector 의 snapshot cache 라
// scrape hot path 에 부담을 주지 않고 in-memory read 만 수행한다.
package api

import (
	"net/http"
	"strconv"
	"strings"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// SnapshotSource 는 noisy neighbor snapshot 을 제공 하는 추상 인터페이스다. exporter.Collector
// 의 Snapshot() 메소드 가 본 인터페이스 를 만족 한다. test 측 에서 fake snapshot 주입 시 사용.
type SnapshotSource interface {
	Snapshot() []correlation.NoisyNeighbor
}

// CrossNodeSnapshotSource 는 #119 의 cross-node interference snapshot 을 제공 하는 추상 인터페이스 다.
// exporter.Collector 의 CrossNodeSnapshot() 메서드 가 본 인터페이스 를 만족 한다. test 측 에서 fake
// snapshot 주입 시 사용 한다.
type CrossNodeSnapshotSource interface {
	CrossNodeSnapshot() []correlation.NodeInterference
}

// ServiceImpactSnapshotSource 는 #148 의 service-impact snapshot 을 제공하는 추상 인터페이스다.
// exporter.Collector 의 ServiceImpactSnapshot() 메서드가 본 인터페이스를 만족한다. test 측에서 fake
// snapshot 주입 시 사용한다.
type ServiceImpactSnapshotSource interface {
	ServiceImpactSnapshot() []correlation.ServiceImpact
}

// CrossLevelSnapshotSource 는 #149 의 cross-level snapshot 을 제공하는 추상 인터페이스다.
// exporter.Collector 의 CrossLevelSnapshot() 메서드가 본 인터페이스를 만족한다. test 측에서 fake
// snapshot 주입 시 사용한다.
type CrossLevelSnapshotSource interface {
	CrossLevelSnapshot() []correlation.CrossLevel
}

// ImpactGraphSnapshotSource 는 #151 의 영향 전파 그래프 (Phase 1) 와 다단계 경로 / 근원 suspect
// (Phase 2) snapshot 을 제공하는 추상 인터페이스다. exporter.Collector 가 세 메서드를 모두 만족하므로
// /api/v1/impact-graph 와 /api/v1/impact-paths 가 동일 source 를 공유한다. test 측에서 fake 주입 시
// 사용한다.
type ImpactGraphSnapshotSource interface {
	ImpactGraphSnapshot() correlation.ImpactGraph
	ImpactPathsSnapshot() []correlation.ImpactPath
	RootSuspectsSnapshot() []correlation.RootSuspect
}

// Handler 는 correlation API endpoint 들 의 의존성을 모은다. SnapshotSource 다섯 종 외 별도 상태가
// 없어 동시 호출 안전 (각 Snapshot() 내부 RLock).
type Handler struct {
	source              SnapshotSource
	crossNodeSource     CrossNodeSnapshotSource
	serviceImpactSource ServiceImpactSnapshotSource
	crossLevelSource    CrossLevelSnapshotSource
	impactGraphSource   ImpactGraphSnapshotSource
}

// NewHandler 는 다섯 SnapshotSource 를 주입 받아 API handler 를 만든다. cmd/correlation-exporter/main.go
// 의 main 함수 에서 exporter.Collector 가 다섯 인터페이스 를 모두 만족 하므로 동일 인스턴스 를 다섯 번
// 전달 한다. crossNodeSource / serviceImpactSource / crossLevelSource / impactGraphSource 가 nil 이면
// 해당 endpoint 가 graceful empty response 를 돌려 준다.
func NewHandler(source SnapshotSource, crossNodeSource CrossNodeSnapshotSource, serviceImpactSource ServiceImpactSnapshotSource, crossLevelSource CrossLevelSnapshotSource, impactGraphSource ImpactGraphSnapshotSource) *Handler {
	return &Handler{source: source, crossNodeSource: crossNodeSource, serviceImpactSource: serviceImpactSource, crossLevelSource: crossLevelSource, impactGraphSource: impactGraphSource}
}

// Register 는 ServeMux 에 /api/v1/noisy-neighbor 와 /api/v1/cross-node-interference 두 라우트 를 등록
// 한다. 호출 측은 mux 를 그대로 전달 하면 된다. 본 함수는 미들웨어 적용 (Logging, Recover, CORS) 도
// 함께 처리 한다.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/noisy-neighbor", apicommon.Chain(
		http.HandlerFunc(h.ListNoisyNeighbors),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/cross-node-interference", apicommon.Chain(
		http.HandlerFunc(h.ListCrossNode),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/service-impact", apicommon.Chain(
		http.HandlerFunc(h.ListServiceImpact),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/cross-level", apicommon.Chain(
		http.HandlerFunc(h.ListCrossLevel),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/impact-graph", apicommon.Chain(
		http.HandlerFunc(h.GetImpactGraph),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/impact-paths", apicommon.Chain(
		http.HandlerFunc(h.ListImpactPaths),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// NoisyNeighborListResponse 는 /api/v1/noisy-neighbor 응답 의 typed 표현 이다. swaggo 가 본 구조체
// 를 OpenAPI schema 로 생성 한다. apicommon.ListResponse 의 Items 가 json.RawMessage 라 docs 측에서
// 는 본 typed 응답 으로 표시 한다.
type NoisyNeighborListResponse struct {
	Items []correlation.NoisyNeighbor `json:"items"`
	Page  apicommon.Page              `json:"page"`
}

// CrossNodeListResponse 는 /api/v1/cross-node-interference 응답 의 typed 표현 이다 (#119). swaggo 가
// 본 구조체 를 OpenAPI schema 로 생성 한다. items 는 NodeInterference 슬라이스, page 는 공용
// pagination 메타데이터 이며 NoisyNeighborListResponse 와 형식 일관성 을 유지 한다.
type CrossNodeListResponse struct {
	Items []correlation.NodeInterference `json:"items"`
	Page  apicommon.Page                 `json:"page"`
}

// ServiceImpactListResponse 는 /api/v1/service-impact 응답의 typed 표현이다 (#148). swaggo 가 본
// 구조체를 OpenAPI schema 로 생성한다. items 는 ServiceImpact 슬라이스, page 는 공용 pagination
// 메타데이터이며 다른 두 List 응답과 형식 일관성을 유지한다.
type ServiceImpactListResponse struct {
	Items []correlation.ServiceImpact `json:"items"`
	Page  apicommon.Page              `json:"page"`
}

// CrossLevelListResponse 는 /api/v1/cross-level 응답의 typed 표현이다 (#149). swaggo 가 본 구조체를
// OpenAPI schema 로 생성한다. items 는 CrossLevel 슬라이스, page 는 공용 pagination 메타데이터이며
// 다른 List 응답과 형식 일관성을 유지한다.
type CrossLevelListResponse struct {
	Items []correlation.CrossLevel `json:"items"`
	Page  apicommon.Page           `json:"page"`
}

// ImpactGraphResponse 는 /api/v1/impact-graph 응답의 typed 표현이다 (#151 Phase 1). 그래프는 페어
// 리스트가 아니라 정점과 엣지로 구성된 단일 객체라 다른 List 응답의 items/page 형식 대신 nodes /
// edges / summary 형식을 쓴다. 그래프 크기가 noisy neighbor Top-N 으로 통제되어 전체를 한 번에
// 반환한다.
type ImpactGraphResponse struct {
	Nodes   []correlation.ImpactGraphNode `json:"nodes"`
	Edges   []correlation.ImpactGraphEdge `json:"edges"`
	Summary ImpactGraphSummary            `json:"summary"`
}

// ImpactGraphSummary 는 그래프 규모 요약이다. dashboard 가 정점 / 엣지 수를 빠르게 파악하게 한다.
type ImpactGraphSummary struct {
	NodeCount int `json:"node_count"`
	EdgeCount int `json:"edge_count"`
}

// ImpactPathsResponse 는 /api/v1/impact-paths 응답의 typed 표현이다 (#151 Phase 2). paths 는 근원
// suspect 에서 종착 victim 으로 이어지는 다단계 경로, roots 는 근원 suspect 별 영향 범위 요약이다.
type ImpactPathsResponse struct {
	Paths   []correlation.ImpactPath  `json:"paths"`
	Roots   []correlation.RootSuspect `json:"roots"`
	Summary ImpactPathsSummary        `json:"summary"`
}

// ImpactPathsSummary 는 경로 / 근원 규모 요약이다.
type ImpactPathsSummary struct {
	PathCount int `json:"path_count"`
	RootCount int `json:"root_count"`
}

// ErrorResponse 는 swaggo cross-package type resolution 한계 회피 용 으로 ErrorResponse 와
// 동일 형태 의 type alias. 본 패키지의 @Failure 주석 이 swag 가 인지 가능한 동일 패키지 타입
// 으로 해석 되도록 둔다.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 은 ErrorResponse 의 nested 필드.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ListNoisyNeighbors 는 /api/v1/noisy-neighbor 의 GET 핸들러 다. 쿼리 파라미터 로 필터링 후
// pagination 적용 결과 를 JSON 응답 으로 반환 한다.
//
// @Summary      List noisy neighbor top-N
// @Description  victim/suspect 와 dimension 과 rank 필터 후 pagination 적용한 noisy neighbor 시리즈 반환
// @Tags         correlation
// @Produce      json
// @Param        victim_namespace   query  string  false  "victim Pod 의 namespace 필터"
// @Param        victim_pod         query  string  false  "victim Pod 의 이름 필터"
// @Param        suspect_namespace  query  string  false  "suspect Pod 의 namespace 필터"
// @Param        suspect_pod        query  string  false  "suspect Pod 의 이름 필터"
// @Param        dimension          query  string  false  "리소스 차원 필터 (cpu/memory/network/gpu)"
// @Param        victim_signal      query  string  false  "victim 영향 종착 차원 필터 (latency/throughput/error)"
// @Param        rank_max           query  int     false  "max rank (기본 무제한)"
// @Param        limit              query  int     false  "응답 item 최대 개수 (기본 100, 최대 1000)"
// @Param        offset             query  int     false  "응답 시작 offset (기본 0)"
// @Success      200  {object}  NoisyNeighborListResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/noisy-neighbor [get]
func (h *Handler) ListNoisyNeighbors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dimension := strings.ToLower(strings.TrimSpace(q.Get("dimension")))
	if dimension != "" && !validDimension(dimension) {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_dimension", "dimension 은 cpu / memory / network / gpu 중 하나여야 합니다")
		return
	}
	victimSignal := strings.ToLower(strings.TrimSpace(q.Get("victim_signal")))
	if victimSignal != "" && !validVictimSignal(victimSignal) {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_victim_signal", "victim_signal 은 latency / throughput / error 중 하나여야 합니다")
		return
	}

	var rankMax int
	if raw := strings.TrimSpace(q.Get("rank_max")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_rank_max", "rank_max 는 양의 정수여야 합니다")
			return
		}
		rankMax = v
	}

	victimNS := strings.TrimSpace(q.Get("victim_namespace"))
	victimPod := strings.TrimSpace(q.Get("victim_pod"))
	suspectNS := strings.TrimSpace(q.Get("suspect_namespace"))
	suspectPod := strings.TrimSpace(q.Get("suspect_pod"))

	// netobs / gpuobs handler 와 동일 패턴 으로 nil source 시 graceful empty response 보장. handler
	// 가 collector 미주입 상태 (예: agent 의 reconcile cycle 시작 전) 에 호출 되어도 panic 회피.
	var all []correlation.NoisyNeighbor
	if h.source != nil {
		all = h.source.Snapshot()
	}
	filtered := make([]correlation.NoisyNeighbor, 0, len(all))
	for _, n := range all {
		if dimension != "" && !strings.EqualFold(string(n.Dimension), dimension) {
			continue
		}
		if victimSignal != "" && !strings.EqualFold(string(n.VictimSignal), victimSignal) {
			continue
		}
		if rankMax > 0 && n.Rank > rankMax {
			continue
		}
		if victimNS != "" && n.Victim.Namespace != victimNS {
			continue
		}
		if victimPod != "" && n.Victim.Pod != victimPod {
			continue
		}
		if suspectNS != "" && n.Suspect.Namespace != suspectNS {
			continue
		}
		if suspectPod != "" && n.Suspect.Pod != suspectPod {
			continue
		}
		filtered = append(filtered, n)
	}

	limit, offset := apicommon.ParsePagination(r)
	paged := apicommon.ApplyPagination(filtered, limit, offset)

	resp := NoisyNeighborListResponse{
		Items: paged,
		Page: apicommon.Page{
			Limit:  limit,
			Offset: offset,
			Total:  len(filtered),
		},
	}
	apicommon.WriteJSON(w, resp)
}

// validDimension 은 쿼리 파라미터 의 dimension 값 검증 한다. correlation.ResourceDimension 의
// 정의 와 정합 한 4 값 만 허용.
func validDimension(d string) bool {
	switch d {
	case "cpu", "memory", "network", "gpu":
		return true
	}
	return false
}

// validVictimSignal 은 #150 의 victim_signal 쿼리 파라미터를 검증한다. correlation.VictimSignal 정의와
// 정합한 세 값만 허용한다.
func validVictimSignal(s string) bool {
	switch s {
	case string(correlation.SignalLatency), string(correlation.SignalThroughput), string(correlation.SignalError):
		return true
	}
	return false
}

// ListCrossNode 는 /api/v1/cross-node-interference 의 GET 핸들러 다 (#119). cross-node interference
// snapshot 을 victim_node 와 suspect_node 와 dimension 과 rank_max filter 적용 후 pagination 응답
// 으로 반환 한다. ListNoisyNeighbors 와 동일 패턴 을 따르며 라벨 셋 만 node-level 로 교체 된다.
//
// @Summary      List cross-node interference top-N
// @Description  victim_node 와 suspect_node 와 dimension 과 rank 필터 후 pagination 적용한 cross-node interference 시리즈 반환
// @Tags         correlation
// @Produce      json
// @Param        victim_node    query  string  false  "victim node 이름 필터"
// @Param        suspect_node   query  string  false  "suspect node 이름 필터"
// @Param        dimension      query  string  false  "리소스 차원 필터 (cpu/memory/network/gpu)"
// @Param        rank_max       query  int     false  "max rank (기본 무제한)"
// @Param        limit          query  int     false  "응답 item 최대 개수 (기본 100, 최대 1000)"
// @Param        offset         query  int     false  "응답 시작 offset (기본 0)"
// @Success      200  {object}  CrossNodeListResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/cross-node-interference [get]
func (h *Handler) ListCrossNode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dimension := strings.ToLower(strings.TrimSpace(q.Get("dimension")))
	if dimension != "" && !validDimension(dimension) {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_dimension", "dimension 은 cpu / memory / network / gpu 중 하나여야 합니다")
		return
	}

	var rankMax int
	if raw := strings.TrimSpace(q.Get("rank_max")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_rank_max", "rank_max 는 양의 정수여야 합니다")
			return
		}
		rankMax = v
	}

	victimNode := strings.TrimSpace(q.Get("victim_node"))
	suspectNode := strings.TrimSpace(q.Get("suspect_node"))

	var all []correlation.NodeInterference
	if h.crossNodeSource != nil {
		all = h.crossNodeSource.CrossNodeSnapshot()
	}
	filtered := make([]correlation.NodeInterference, 0, len(all))
	for _, n := range all {
		if dimension != "" && !strings.EqualFold(string(n.Dimension), dimension) {
			continue
		}
		if rankMax > 0 && n.Rank > rankMax {
			continue
		}
		if victimNode != "" && n.VictimNode != victimNode {
			continue
		}
		if suspectNode != "" && n.SuspectNode != suspectNode {
			continue
		}
		filtered = append(filtered, n)
	}

	limit, offset := apicommon.ParsePagination(r)
	paged := apicommon.ApplyPagination(filtered, limit, offset)
	// apicommon.ApplyPagination 이 빈 입력 또는 offset 초과 시 nil 슬라이스 를 반환 할 수 있어
	// JSON 직렬화 가 "items": null 로 떨어질 위험 이 있다. graceful empty response 보장 위해
	// nil 시 빈 슬라이스 로 정규화 해 응답 schema 가 항상 "items": [] 형태 를 유지 하게 한다.
	if paged == nil {
		paged = []correlation.NodeInterference{}
	}

	resp := CrossNodeListResponse{
		Items: paged,
		Page: apicommon.Page{
			Limit:  limit,
			Offset: offset,
			Total:  len(filtered),
		},
	}
	apicommon.WriteJSON(w, resp)
}

// ListServiceImpact 는 /api/v1/service-impact 의 GET 핸들러다 (#148). service-impact snapshot 을
// victim_namespace 와 victim_workload 와 suspect_node 와 dimension 과 rank_max filter 적용 후
// pagination 응답으로 반환한다. ListCrossNode 와 동일 패턴을 따르며 라벨 셋만 workload-level victim
// 으로 교체된다.
//
// @Summary      List service-impact top-N
// @Description  victim_namespace 와 victim_workload 와 suspect_node 와 dimension 과 rank 필터 후 pagination 적용한 service-impact 시리즈 반환
// @Tags         correlation
// @Produce      json
// @Param        victim_namespace  query  string  false  "victim workload 의 namespace 필터"
// @Param        victim_workload   query  string  false  "victim workload (Service 근사) 이름 필터"
// @Param        suspect_node      query  string  false  "suspect node 이름 필터"
// @Param        dimension         query  string  false  "리소스 차원 필터 (cpu/memory/network/gpu)"
// @Param        rank_max          query  int     false  "max rank (기본 무제한)"
// @Param        limit             query  int     false  "응답 item 최대 개수 (기본 100, 최대 1000)"
// @Param        offset            query  int     false  "응답 시작 offset (기본 0)"
// @Success      200  {object}  ServiceImpactListResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/service-impact [get]
func (h *Handler) ListServiceImpact(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dimension := strings.ToLower(strings.TrimSpace(q.Get("dimension")))
	if dimension != "" && !validDimension(dimension) {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_dimension", "dimension 은 cpu / memory / network / gpu 중 하나여야 합니다")
		return
	}

	var rankMax int
	if raw := strings.TrimSpace(q.Get("rank_max")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_rank_max", "rank_max 는 양의 정수여야 합니다")
			return
		}
		rankMax = v
	}

	victimNS := strings.TrimSpace(q.Get("victim_namespace"))
	victimWorkload := strings.TrimSpace(q.Get("victim_workload"))
	suspectNode := strings.TrimSpace(q.Get("suspect_node"))

	var all []correlation.ServiceImpact
	if h.serviceImpactSource != nil {
		all = h.serviceImpactSource.ServiceImpactSnapshot()
	}
	filtered := make([]correlation.ServiceImpact, 0, len(all))
	for _, s := range all {
		if dimension != "" && !strings.EqualFold(string(s.Dimension), dimension) {
			continue
		}
		if rankMax > 0 && s.Rank > rankMax {
			continue
		}
		if victimNS != "" && s.VictimNamespace != victimNS {
			continue
		}
		if victimWorkload != "" && s.VictimWorkload != victimWorkload {
			continue
		}
		if suspectNode != "" && s.SuspectNode != suspectNode {
			continue
		}
		filtered = append(filtered, s)
	}

	limit, offset := apicommon.ParsePagination(r)
	paged := apicommon.ApplyPagination(filtered, limit, offset)
	// apicommon.ApplyPagination 이 빈 입력 또는 offset 초과 시 nil 슬라이스를 반환할 수 있어 JSON
	// 직렬화가 "items": null 로 떨어질 위험이 있다. graceful empty response 보장을 위해 nil 시 빈
	// 슬라이스로 정규화해 응답 schema 가 항상 "items": [] 형태를 유지하게 한다.
	if paged == nil {
		paged = []correlation.ServiceImpact{}
	}

	resp := ServiceImpactListResponse{
		Items: paged,
		Page: apicommon.Page{
			Limit:  limit,
			Offset: offset,
			Total:  len(filtered),
		},
	}
	apicommon.WriteJSON(w, resp)
}

// validDirection 은 쿼리 파라미터의 direction 값을 검증한다. CrossLevelDirection 정의와 정합한 두
// 값만 허용한다.
func validDirection(d string) bool {
	switch d {
	case string(correlation.DirectionNodeToPod), string(correlation.DirectionPodToNode):
		return true
	}
	return false
}

// ListCrossLevel 은 /api/v1/cross-level 의 GET 핸들러다 (#149). cross-level snapshot 을 node 와 pod_
// namespace 와 pod 와 direction 과 dimension 과 rank_max filter 적용 후 pagination 응답으로 반환한다.
// 다른 List 핸들러와 동일 패턴을 따르며 라벨 셋만 cross-level 로 교체된다.
//
// @Summary      List cross-level interference top-N
// @Description  node 와 pod 와 direction 과 dimension 과 rank 필터 후 pagination 적용한 cross-level 시리즈 반환
// @Tags         correlation
// @Produce      json
// @Param        node            query  string  false  "공유 node 이름 필터"
// @Param        pod_namespace   query  string  false  "pod 의 namespace 필터"
// @Param        pod             query  string  false  "pod 이름 필터"
// @Param        direction       query  string  false  "방향 필터 (node_to_pod/pod_to_node)"
// @Param        dimension       query  string  false  "리소스 차원 필터 (cpu/memory/network/gpu)"
// @Param        rank_max        query  int     false  "max rank (기본 무제한)"
// @Param        limit           query  int     false  "응답 item 최대 개수 (기본 100, 최대 1000)"
// @Param        offset          query  int     false  "응답 시작 offset (기본 0)"
// @Success      200  {object}  CrossLevelListResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/cross-level [get]
func (h *Handler) ListCrossLevel(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dimension := strings.ToLower(strings.TrimSpace(q.Get("dimension")))
	if dimension != "" && !validDimension(dimension) {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_dimension", "dimension 은 cpu / memory / network / gpu 중 하나여야 합니다")
		return
	}
	direction := strings.ToLower(strings.TrimSpace(q.Get("direction")))
	if direction != "" && !validDirection(direction) {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_direction", "direction 은 node_to_pod / pod_to_node 중 하나여야 합니다")
		return
	}

	var rankMax int
	if raw := strings.TrimSpace(q.Get("rank_max")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_rank_max", "rank_max 는 양의 정수여야 합니다")
			return
		}
		rankMax = v
	}

	node := strings.TrimSpace(q.Get("node"))
	podNamespace := strings.TrimSpace(q.Get("pod_namespace"))
	pod := strings.TrimSpace(q.Get("pod"))

	var all []correlation.CrossLevel
	if h.crossLevelSource != nil {
		all = h.crossLevelSource.CrossLevelSnapshot()
	}
	filtered := make([]correlation.CrossLevel, 0, len(all))
	for _, cl := range all {
		if dimension != "" && !strings.EqualFold(string(cl.Dimension), dimension) {
			continue
		}
		if direction != "" && !strings.EqualFold(string(cl.Direction), direction) {
			continue
		}
		if rankMax > 0 && cl.Rank > rankMax {
			continue
		}
		if node != "" && cl.Node != node {
			continue
		}
		if podNamespace != "" && cl.PodNamespace != podNamespace {
			continue
		}
		if pod != "" && cl.Pod != pod {
			continue
		}
		filtered = append(filtered, cl)
	}

	limit, offset := apicommon.ParsePagination(r)
	paged := apicommon.ApplyPagination(filtered, limit, offset)
	// apicommon.ApplyPagination 이 빈 입력 또는 offset 초과 시 nil 슬라이스를 반환할 수 있어 JSON
	// 직렬화가 "items": null 로 떨어질 위험이 있다. graceful empty response 보장을 위해 nil 시 빈
	// 슬라이스로 정규화해 응답 schema 가 항상 "items": [] 형태를 유지하게 한다.
	if paged == nil {
		paged = []correlation.CrossLevel{}
	}

	resp := CrossLevelListResponse{
		Items: paged,
		Page: apicommon.Page{
			Limit:  limit,
			Offset: offset,
			Total:  len(filtered),
		},
	}
	apicommon.WriteJSON(w, resp)
}

// GetImpactGraph 는 /api/v1/impact-graph 의 GET 핸들러다 (#151 Phase 1). 영향 전파 그래프의 정점과
// 엣지 전체를 nodes / edges / summary 형식으로 반환한다. 그래프 크기가 noisy neighbor Top-N 으로
// 통제되어 pagination 없이 한 번에 돌려 준다. 첫 reconcile 전이거나 ImpactGraphEnabled=false 면 빈
// 그래프를 graceful 하게 반환한다. transitive 경로 추출과 필터는 Phase 2 에서 추가한다.
//
// @Summary      Get impact propagation graph
// @Description  suspect → victim 1-hop 엣지로 구성된 영향 전파 그래프의 정점 (out/in degree 포함) 과 엣지 반환. namespace / min_score 로 유도 부분그래프를 추릴 수 있다
// @Tags         correlation
// @Produce      json
// @Param        namespace  query  string  false  "suspect 또는 victim 이 이 namespace 인 엣지만 (유도 부분그래프)"
// @Param        min_score  query  number  false  "score 가 이 값 이상인 엣지만"
// @Success      200  {object}  ImpactGraphResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/impact-graph [get]
func (h *Handler) GetImpactGraph(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	namespace := strings.TrimSpace(q.Get("namespace"))
	var minScore float64
	if raw := strings.TrimSpace(q.Get("min_score")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_min_score", "min_score 는 실수여야 합니다")
			return
		}
		minScore = v
	}

	var g correlation.ImpactGraph
	if h.impactGraphSource != nil {
		g = h.impactGraphSource.ImpactGraphSnapshot()
	}
	if namespace != "" || minScore > 0 {
		g = g.Filter(namespace, minScore)
	}
	// nil 슬라이스는 JSON 에서 null 로 직렬화되므로 빈 슬라이스로 정규화해 응답이 항상 nodes:[] /
	// edges:[] 형태를 유지하게 한다 (graceful empty).
	if g.Nodes == nil {
		g.Nodes = []correlation.ImpactGraphNode{}
	}
	if g.Edges == nil {
		g.Edges = []correlation.ImpactGraphEdge{}
	}
	apicommon.WriteJSON(w, ImpactGraphResponse{
		Nodes: g.Nodes,
		Edges: g.Edges,
		Summary: ImpactGraphSummary{
			NodeCount: len(g.Nodes),
			EdgeCount: len(g.Edges),
		},
	})
}

// ListImpactPaths 는 /api/v1/impact-paths 의 GET 핸들러다 (#151 Phase 2). 근원 suspect 에서 종착
// victim 으로 이어지는 다단계 전파 경로와 근원 suspect 요약 (roots) 을 반환한다. root_pod /
// terminal_pod / namespace / min_score 필터로 특정 근원·종착·강도의 경로만 추릴 수 있다. 그래프 크기가
// 통제되어 pagination 없이 반환하며 빈 결과는 graceful empty 다.
//
// @Summary      List multi-hop impact propagation paths
// @Description  근원 suspect(root)에서 종착 victim(terminal)으로 이어지는 다단계 경로와 근원 요약 반환. root_pod / terminal_pod / namespace / min_score 필터 지원
// @Tags         correlation
// @Produce      json
// @Param        root_pod      query  string   false  "근원 suspect pod 이름 필터"
// @Param        terminal_pod  query  string   false  "종착 victim pod 이름 필터"
// @Param        namespace     query  string   false  "root 또는 terminal 의 namespace 필터"
// @Param        min_score     query  number   false  "경로 weakest-link score 하한 필터"
// @Success      200  {object}  ImpactPathsResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/impact-paths [get]
func (h *Handler) ListImpactPaths(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rootPod := strings.TrimSpace(q.Get("root_pod"))
	terminalPod := strings.TrimSpace(q.Get("terminal_pod"))
	namespace := strings.TrimSpace(q.Get("namespace"))

	var minScore float64
	if raw := strings.TrimSpace(q.Get("min_score")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			apicommon.WriteError(w, http.StatusBadRequest, "invalid_min_score", "min_score 는 실수여야 합니다")
			return
		}
		minScore = v
	}

	var paths []correlation.ImpactPath
	var roots []correlation.RootSuspect
	if h.impactGraphSource != nil {
		paths = h.impactGraphSource.ImpactPathsSnapshot()
		roots = h.impactGraphSource.RootSuspectsSnapshot()
	}
	filtered := make([]correlation.ImpactPath, 0, len(paths))
	for _, p := range paths {
		if rootPod != "" && p.Root.Pod != rootPod {
			continue
		}
		if terminalPod != "" && p.Terminal.Pod != terminalPod {
			continue
		}
		if namespace != "" && p.Root.Namespace != namespace && p.Terminal.Namespace != namespace {
			continue
		}
		if minScore > 0 && p.Score < minScore {
			continue
		}
		filtered = append(filtered, p)
	}
	if filtered == nil {
		filtered = []correlation.ImpactPath{}
	}
	if roots == nil {
		roots = []correlation.RootSuspect{}
	}

	apicommon.WriteJSON(w, ImpactPathsResponse{
		Paths: filtered,
		Roots: roots,
		Summary: ImpactPathsSummary{
			PathCount: len(filtered),
			RootCount: len(roots),
		},
	})
}
