// Package api 는 이슈 #100 의 자체 dashboard 용 REST API layer 의 gpuobs-agent 측 구현이다.
// /api/v1/gpu endpoint 를 노출 해 device scope 와 pod scope 자원 사용량 을 JSON 으로 직접 조회
// 가능 하게 한다.
//
// 현재 구현 상태: skeleton handler. GPU device 와 Pod scope 의 in-memory snapshot helper 추출
// 작업은 follow-up 이슈 로 위임 하고 본 PR 은 API layer 인프라 와 OpenAPI spec 정의 까지 를 scope
// 로 둔다.
package api

import (
	"net/http"
	"strings"

	"netobs/internal/apicommon"
)

// GPUSource 는 GPU scope 별 snapshot 추상 인터페이스. 실 source 연결 follow-up 에서 gpuobs
// collector 가 본 인터페이스 를 구현 한다.
type GPUSource interface {
	SnapshotGPU(scope string) []GPUEntry
}

// GPUEntry 는 device scope 또는 pod scope 의 단일 GPU record.
type GPUEntry struct {
	Scope             string  `json:"scope"`
	Node              string  `json:"node"`
	GPUUUID           string  `json:"gpu_uuid"`
	GPUIndex          string  `json:"gpu_index"`
	GPUModel          string  `json:"gpu_model"`
	SrcNamespace      string  `json:"src_namespace,omitempty"`
	SrcPod            string  `json:"src_pod,omitempty"`
	UtilizationPercent float64 `json:"utilization_percent"`
	MemoryUsedBytes   uint64  `json:"memory_used_bytes"`
	MemoryTotalBytes  uint64  `json:"memory_total_bytes"`
	TemperatureCelsius float64 `json:"temperature_celsius"`
	PowerUsageWatts   float64 `json:"power_usage_watts"`
	PowerLimitWatts   float64 `json:"power_limit_watts"`
}

// Handler 는 gpuobs API endpoint 의 의존성 모음.
type Handler struct {
	source GPUSource
}

// NewHandler 는 GPUSource 를 주입 받아 handler 를 만든다.
func NewHandler(source GPUSource) *Handler {
	return &Handler{source: source}
}

// Register 는 ServeMux 에 /api/v1/gpu 라우트 를 등록.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/gpu", apicommon.Chain(
		http.HandlerFunc(h.ListGPU),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// PageInfo 는 apicommon.Page 의 local alias.
type PageInfo struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// GPUListResponse 는 /api/v1/gpu 응답 의 typed 표현.
type GPUListResponse struct {
	Items []GPUEntry `json:"items"`
	Page  PageInfo   `json:"page"`
}

// ErrorResponse 는 swaggo cross-package type resolution 한계 회피 용 type alias.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 은 ErrorResponse 의 nested 필드.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ListGPU 는 /api/v1/gpu GET 핸들러. scope 쿼리 파라미터로 device 또는 pod scope 선택.
//
// @Summary      List GPU resource usage
// @Description  device scope 또는 pod scope 의 GPU utilization 과 memory 와 temperature 와 power 통합 조회
// @Tags         gpuobs
// @Produce      json
// @Param        scope          query  string  false  "scope (device 또는 pod, 기본 device)"
// @Param        node           query  string  false  "node 필터"
// @Param        gpu_uuid       query  string  false  "gpu_uuid 필터"
// @Param        src_namespace  query  string  false  "Pod 의 namespace 필터 (scope=pod 일 때만 유효)"
// @Param        src_pod        query  string  false  "Pod 의 이름 필터 (scope=pod 일 때만 유효)"
// @Param        limit          query  int     false  "응답 item 최대 개수 (기본 100, 최대 1000)"
// @Param        offset         query  int     false  "응답 시작 offset (기본 0)"
// @Success      200  {object}  GPUListResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/gpu [get]
func (h *Handler) ListGPU(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	scope := strings.ToLower(strings.TrimSpace(q.Get("scope")))
	if scope == "" {
		scope = "device"
	}
	if scope != "device" && scope != "pod" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_scope", "scope 는 device 또는 pod 여야 합니다")
		return
	}

	node := strings.TrimSpace(q.Get("node"))
	gpuUUID := strings.TrimSpace(q.Get("gpu_uuid"))
	srcNS := strings.TrimSpace(q.Get("src_namespace"))
	srcPod := strings.TrimSpace(q.Get("src_pod"))

	var all []GPUEntry
	if h.source != nil {
		all = h.source.SnapshotGPU(scope)
	}
	filtered := make([]GPUEntry, 0, len(all))
	for _, e := range all {
		if node != "" && e.Node != node {
			continue
		}
		if gpuUUID != "" && e.GPUUUID != gpuUUID {
			continue
		}
		if scope == "pod" {
			if srcNS != "" && e.SrcNamespace != srcNS {
				continue
			}
			if srcPod != "" && e.SrcPod != srcPod {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	limit, offset := apicommon.ParsePagination(r)
	paged := apicommon.ApplyPagination(filtered, limit, offset)
	apicommon.WriteJSON(w, GPUListResponse{
		Items: paged,
		Page:  PageInfo{Limit: limit, Offset: offset, Total: len(filtered)},
	})
}
