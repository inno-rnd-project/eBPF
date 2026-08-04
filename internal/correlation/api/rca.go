package api

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"netobs/internal/apicommon"
)

// RCAProxyHandler 는 #234 의 RCA 요약 단일 진입점이다. rca-summarizer 가 Alertmanager webhook 으로
// 생성한 RCASummary 를 correlation-exporter API 표면에서 조회 가능하게 GET /rca 를 프록시한다.
// 프론트엔드가 rca-summarizer 서비스의 존재를 몰라도 되고, NetworkPolicy 소비 경로도 correlation
// 하나로 유지된다.
type RCAProxyHandler struct {
	base   *url.URL
	client *http.Client
}

// NewRCAProxyHandler 는 rca-summarizer base URL (예: http://rca-summarizer.ebpf-project.svc:9850)
// 로 프록시를 구성한다. URL 파싱 실패 시 nil 을 돌려주고 호출자는 라우트 등록을 skip 한다.
func NewRCAProxyHandler(baseURL string, timeout time.Duration) *RCAProxyHandler {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return nil
	}
	return &RCAProxyHandler{base: u, client: &http.Client{Timeout: timeout}}
}

// Register 는 /api/v1/rca 라우트를 mux 에 등록한다.
func (h *RCAProxyHandler) Register(mux *http.ServeMux) {
	mux.Handle("/api/v1/rca", apicommon.Chain(
		http.HandlerFunc(h.GetRCA),
		apicommon.LoggingMiddleware,
		apicommon.RecoverMiddleware,
		apicommon.MethodGuard,
		apicommon.CORSMiddleware,
	))
}

// GetRCA godoc
// @Summary      RCA 요약 조회
// @Description  rca-summarizer 가 Alertmanager webhook 으로 생성한 alert 별 root cause analysis 요약을 프록시한다. alert 파라미터 생략 시 보관 중인 전체 요약 목록, 지정 시 해당 alert 의 최신 요약을 돌려준다 (미보관 alert 는 404). 요약에는 지배 자원 차원과 최우선 의심 pod, 핵심 drop flow, 근거 메트릭 힌트, 신뢰도 점수가 담긴다.
// @Tags         rca
// @Produce      json
// @Param        alert  query  string  false  "alertname (생략 시 전체 목록)"
// @Success      200  {object}  map[string]interface{}
// @Failure      404  {object}  apicommon.ErrorBody
// @Failure      502  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/rca [get]
func (h *RCAProxyHandler) GetRCA(w http.ResponseWriter, r *http.Request) {
	// ResolveReference 로 경로를 구성해 base 에 인코딩된 경로가 있어도 RawPath 잔존 없이 안전하게
	// /rca 로 해석되게 한다.
	u := h.base.ResolveReference(&url.URL{Path: "/rca"})
	// alert 파라미터만 통과시켜 임의 파라미터 주입을 차단한다.
	v := url.Values{}
	if alert := strings.TrimSpace(r.URL.Query().Get("alert")); alert != "" {
		v.Set("alert", alert)
	}
	u.RawQuery = v.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "proxy_build_failed", err.Error())
		return
	}
	resp, err := h.client.Do(req)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadGateway, "rca_unreachable", "rca-summarizer 호출 실패: "+err.Error())
		return
	}
	defer resp.Body.Close()

	// 상류의 Content-Type 을 그대로 전달해 비JSON 에러 본문이 JSON 으로 오표기되지 않게 하고,
	// 헤더 부재 시에만 기본 JSON 으로 둔다.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}
