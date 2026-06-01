// Package api 는 이슈 #100 의 자체 dashboard 용 REST API layer 의 correlation-exporter 측
// 구현이다. 운영자 또는 자체 dashboard 가 Prometheus query 없이 noisy neighbor top-N 결과를
// JSON 으로 직접 조회 가능 하게 한다. 데이터 source 는 exporter.Collector 의 snapshot cache 라
// scrape hot path 에 부담을 주지 않고 in-memory read 만 수행한다.
package api

import (
	"encoding/json"
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

// Handler 는 noisy-neighbor API endpoint 의 의존성을 모은다. SnapshotSource 외 별도 상태가 없어
// 동시 호출 안전 (Snapshot() 내부 RLock).
type Handler struct {
	source SnapshotSource
}

// NewHandler 는 SnapshotSource 를 주입 받아 API handler 를 만든다. cmd/correlation-exporter/main.go
// 의 main 함수 에서 exporter.Collector 를 그대로 전달 한다.
func NewHandler(source SnapshotSource) *Handler {
	return &Handler{source: source}
}

// Register 는 ServeMux 에 /api/v1/noisy-neighbor 라우트 를 등록 한다. 호출 측은 mux 를 그대로
// 전달 하면 된다. 본 함수는 미들웨어 적용 (Logging, Recover, CORS) 도 함께 처리 한다.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/noisy-neighbor", apicommon.Chain(
		http.HandlerFunc(h.ListNoisyNeighbors),
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

	all := h.source.Snapshot()
	filtered := make([]correlation.NoisyNeighbor, 0, len(all))
	for _, n := range all {
		if dimension != "" && !strings.EqualFold(string(n.Dimension), dimension) {
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
	// apicommon.WriteJSON 은 generic any 라 typed marshal 직접 호출 후 raw 응답을 보장한다.
	_ = json.NewEncoder(w).Encode // suppress unused (apicommon.WriteJSON 호출 시 자동 사용)
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
