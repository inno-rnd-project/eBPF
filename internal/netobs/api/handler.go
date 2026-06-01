// Package api 는 이슈 #100 의 자체 dashboard 용 REST API layer 의 netobs-agent 측 구현이다.
// /api/v1/flows 와 /api/v1/drops 두 endpoint 를 노출 한다.
//
// 현재 구현 상태: skeleton handler. BPF map iterate 의 in-memory snapshot helper 추출 작업은
// follow-up 이슈 (#100 의 후속) 로 위임 한다. 본 endpoint 는 API layer 인프라 (라우터, swagger,
// pagination, 미들웨어) 와 OpenAPI spec 정의 까지를 본 PR scope 로 둔다. 실 source 연결 후
// 응답 의 items 가 자연 채워 진다.
package api

import (
	"net/http"
	"strings"

	"netobs/internal/apicommon"
)

// FlowSource 는 flow snapshot 의 추상 인터페이스. 실 source 연결 follow-up 에서 flow.Collector
// 가 본 인터페이스 를 구현 한다. nil source 는 graceful empty response.
type FlowSource interface {
	SnapshotFlows() []FlowEntry
}

// DropSource 는 drop event snapshot 의 추상 인터페이스.
type DropSource interface {
	SnapshotDrops() []DropEntry
}

// FlowEntry 는 5-tuple flow 의 단일 record. 본 PR 에서 OpenAPI schema 노출 만 담당 하고 실
// source 가 채워질 때 본 타입 으로 marshal 된다.
type FlowEntry struct {
	Node          string `json:"node"`
	SrcNamespace  string `json:"src_namespace"`
	SrcPod        string `json:"src_pod"`
	SrcIP         string `json:"src_ip"`
	SrcPort       uint16 `json:"src_port"`
	DstNamespace  string `json:"dst_namespace"`
	DstPod        string `json:"dst_pod"`
	DstIP         string `json:"dst_ip"`
	DstPort       uint16 `json:"dst_port"`
	Protocol      string `json:"protocol"`
	Direction     string `json:"direction"`
	BytesTotal    uint64 `json:"bytes_total"`
	BytesPerSec   float64 `json:"bytes_per_sec"`
}

// DropEntry 는 drop event 의 단일 record.
type DropEntry struct {
	Node          string `json:"node"`
	SrcNamespace  string `json:"src_namespace"`
	SrcPod        string `json:"src_pod"`
	SrcIP         string `json:"src_ip"`
	SrcPort       uint16 `json:"src_port"`
	DstIP         string `json:"dst_ip"`
	DstPort       uint16 `json:"dst_port"`
	Protocol      string `json:"protocol"`
	DropReason    string `json:"drop_reason"`
	DropCategory  string `json:"drop_category"`
	EventCount    uint64 `json:"event_count"`
	EventsPerSec  float64 `json:"events_per_sec"`
}

// Handler 는 netobs-agent 의 API endpoint 의존성 모음.
type Handler struct {
	flows FlowSource
	drops DropSource
}

// NewHandler 는 FlowSource 와 DropSource 를 주입 받아 handler 를 만든다. 둘 다 nil 이면 빈
// 응답 반환 형태로 graceful degradation.
func NewHandler(flows FlowSource, drops DropSource) *Handler {
	return &Handler{flows: flows, drops: drops}
}

// Register 는 ServeMux 에 두 endpoint 를 등록.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/flows", apicommon.Chain(
		http.HandlerFunc(h.ListFlows),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
	mux.Handle("/api/v1/drops", apicommon.Chain(
		http.HandlerFunc(h.ListDrops),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.CORSMiddleware,
	))
}

// PageInfo 는 apicommon.Page 의 local alias. swaggo cross-package type resolution 한계 회피용.
type PageInfo struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// FlowListResponse 는 /api/v1/flows 응답 의 typed 표현.
type FlowListResponse struct {
	Items []FlowEntry `json:"items"`
	Page  PageInfo    `json:"page"`
}

// DropListResponse 는 /api/v1/drops 응답 의 typed 표현.
type DropListResponse struct {
	Items []DropEntry `json:"items"`
	Page  PageInfo    `json:"page"`
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

// ListFlows 는 /api/v1/flows GET 핸들러.
//
// @Summary      List Pod-to-Pod network flows
// @Description  5-tuple 기준 flow bytes 와 rate 조회. namespace/pod/protocol/direction 필터 후 pagination 적용
// @Tags         netobs
// @Produce      json
// @Param        src_namespace  query  string  false  "src Pod 의 namespace 필터"
// @Param        src_pod        query  string  false  "src Pod 의 이름 필터"
// @Param        dst_namespace  query  string  false  "dst Pod 의 namespace 필터"
// @Param        dst_pod        query  string  false  "dst Pod 의 이름 필터"
// @Param        protocol       query  string  false  "프로토콜 필터 (tcp/udp)"
// @Param        direction      query  string  false  "방향 필터 (egress/ingress)"
// @Param        limit          query  int     false  "응답 item 최대 개수 (기본 100, 최대 1000)"
// @Param        offset         query  int     false  "응답 시작 offset (기본 0)"
// @Success      200  {object}  FlowListResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/flows [get]
func (h *Handler) ListFlows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	srcNS := strings.TrimSpace(q.Get("src_namespace"))
	srcPod := strings.TrimSpace(q.Get("src_pod"))
	dstNS := strings.TrimSpace(q.Get("dst_namespace"))
	dstPod := strings.TrimSpace(q.Get("dst_pod"))
	protocol := strings.ToLower(strings.TrimSpace(q.Get("protocol")))
	direction := strings.ToLower(strings.TrimSpace(q.Get("direction")))

	if protocol != "" && protocol != "tcp" && protocol != "udp" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_protocol", "protocol 은 tcp 또는 udp 여야 합니다")
		return
	}
	if direction != "" && direction != "egress" && direction != "ingress" {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_direction", "direction 은 egress 또는 ingress 여야 합니다")
		return
	}

	var all []FlowEntry
	if h.flows != nil {
		all = h.flows.SnapshotFlows()
	}
	filtered := make([]FlowEntry, 0, len(all))
	for _, f := range all {
		if srcNS != "" && f.SrcNamespace != srcNS {
			continue
		}
		if srcPod != "" && f.SrcPod != srcPod {
			continue
		}
		if dstNS != "" && f.DstNamespace != dstNS {
			continue
		}
		if dstPod != "" && f.DstPod != dstPod {
			continue
		}
		if protocol != "" && !strings.EqualFold(f.Protocol, protocol) {
			continue
		}
		if direction != "" && !strings.EqualFold(f.Direction, direction) {
			continue
		}
		filtered = append(filtered, f)
	}
	limit, offset := apicommon.ParsePagination(r)
	paged := apicommon.ApplyPagination(filtered, limit, offset)
	apicommon.WriteJSON(w, FlowListResponse{
		Items: paged,
		Page:  PageInfo{Limit: limit, Offset: offset, Total: len(filtered)},
	})
}

// ListDrops 는 /api/v1/drops GET 핸들러.
//
// @Summary      List packet drop events
// @Description  drop_reason 별 5-tuple 분포 조회. namespace 와 reason 필터 후 pagination 적용
// @Tags         netobs
// @Produce      json
// @Param        drop_reason    query  string  false  "drop reason 필터"
// @Param        drop_category  query  string  false  "drop category 필터"
// @Param        src_namespace  query  string  false  "src Pod 의 namespace 필터"
// @Param        src_pod        query  string  false  "src Pod 의 이름 필터"
// @Param        limit          query  int     false  "응답 item 최대 개수 (기본 100, 최대 1000)"
// @Param        offset         query  int     false  "응답 시작 offset (기본 0)"
// @Success      200  {object}  DropListResponse
// @Failure      400  {object}  ErrorResponse
// @Router       /api/v1/drops [get]
func (h *Handler) ListDrops(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	reason := strings.TrimSpace(q.Get("drop_reason"))
	category := strings.TrimSpace(q.Get("drop_category"))
	srcNS := strings.TrimSpace(q.Get("src_namespace"))
	srcPod := strings.TrimSpace(q.Get("src_pod"))

	var all []DropEntry
	if h.drops != nil {
		all = h.drops.SnapshotDrops()
	}
	filtered := make([]DropEntry, 0, len(all))
	for _, d := range all {
		if reason != "" && d.DropReason != reason {
			continue
		}
		if category != "" && d.DropCategory != category {
			continue
		}
		if srcNS != "" && d.SrcNamespace != srcNS {
			continue
		}
		if srcPod != "" && d.SrcPod != srcPod {
			continue
		}
		filtered = append(filtered, d)
	}
	limit, offset := apicommon.ParsePagination(r)
	paged := apicommon.ApplyPagination(filtered, limit, offset)
	apicommon.WriteJSON(w, DropListResponse{
		Items: paged,
		Page:  PageInfo{Limit: limit, Offset: offset, Total: len(filtered)},
	})
}
