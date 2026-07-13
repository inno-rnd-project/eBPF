package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"netobs/internal/apicommon"
	gpuobstypes "netobs/internal/gpuobs/types"
)

// gpu-processes 는 #281 의 노드 GPU 실행 프로세스 프록시 API 다. PID 라벨은 카디널리티 때문에
// 메트릭으로 노출할 수 없어, gpuobs agent 의 로컬 /processes 스냅샷을 correlation-exporter 가 단일
// 진입점으로 중계한다. 노드별 agent 주소는 Prometheus `up` 시리즈의 instance 라벨 (ServiceMonitor
// scrape 대상 = pod IP:port) 로 해석해 kube client 나 headless Service 없이 기존 인프라를 재사용한다.
// Prometheus 를 거치지 않는 실시간 스냅샷 경로라 agent 미응답은 빈 결과와 사유로 graceful 처리한다.

// GpuProcessesResponse 는 GET /api/v1/gpu-processes 의 typed 응답이다.
type GpuProcessesResponse struct {
	GeneratedAt string `json:"generated_at"`
	Node        string `json:"node"`
	// Available 은 agent 스냅샷 확보 여부다. false 면 Reason 에 사유가 담기고 processes 는 빈다.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// CollectedAt 은 agent 가 스냅샷을 만든 poll 시각 (RFC3339) 이다.
	CollectedAt string                         `json:"collected_at,omitempty"`
	Processes   []gpuobstypes.GPUProcessDetail `json:"processes"`
	Summary     string                         `json:"summary"`
}

// GetGpuProcesses godoc
// @Summary      노드 GPU 실행 프로세스 목록 (agent 프록시)
// @Description  노드의 gpuobs agent 로컬 /processes 스냅샷 (PID, GPU device, compute/graphics 타입, GPU 메모리, cgroup 기반 소유 pod, best-effort SM util) 을 단일 진입점으로 중계한다. agent 주소는 Prometheus up 시리즈의 instance 라벨로 해석하며, agent 미존재 / down / 미응답은 빈 목록과 사유 (available=false, reason) 로 graceful 처리한다. 실시간 스냅샷 경로라 at 시점 재구성은 지원하지 않는다.
// @Tags         gpu
// @Produce      json
// @Param        node  query  string  true  "대상 노드 (DNS-1123 형식)"
// @Success      200  {object}  GpuProcessesResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/gpu-processes [get]
func (h *SynthesisHandler) GetGpuProcesses(w http.ResponseWriter, r *http.Request) {
	node, err := parseNodeParam(strings.TrimSpace(r.URL.Query().Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	if node == "" {
		apicommon.WriteError(w, http.StatusBadRequest, "missing_node", "node 파라미터가 필요합니다")
		return
	}

	resp := GpuProcessesResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Node:        node,
		Processes:   []gpuobstypes.GPUProcessDetail{},
	}
	writeOut := func() {
		resp.Summary = buildGpuProcessesSummary(resp)
		apicommon.WriteJSON(w, resp)
	}
	if h.querier == nil {
		resp.Reason = "querier 미구성"
		writeOut()
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// agent 주소 해석. job 매처는 고정 리터럴이고 node 는 parseNodeParam 검증을 통과한 값이라
	// %q 결합이 안전하다. 조회 실패는 인프라 (Prometheus) 문제라 500 으로 구분한다.
	up, err := h.querier.Query(ctx, fmt.Sprintf(`up{job="gpuobs-agent", node=%q}`, node))
	if err != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
		return
	}
	if len(up) == 0 {
		resp.Reason = "노드에 gpuobs agent 스크레이프 대상이 없습니다"
		writeOut()
		return
	}
	if up[0].Value != 1 {
		resp.Reason = "gpuobs agent 가 down 상태입니다 (up=0)"
		writeOut()
		return
	}
	instance := up[0].Labels["instance"]
	if !validAgentInstance(instance) {
		resp.Reason = fmt.Sprintf("agent instance 주소 형식이 유효하지 않습니다: %q", instance)
		writeOut()
		return
	}

	listing, err := h.fetchAgentProcesses(ctx, instance)
	if err != nil {
		resp.Reason = fmt.Sprintf("agent 응답 실패: %v", err)
		writeOut()
		return
	}
	resp.Available = true
	resp.CollectedAt = listing.CollectedAt
	if listing.Processes != nil {
		resp.Processes = listing.Processes
	}
	writeOut()
}

// validAgentInstance 는 Prometheus instance 라벨이 "IP:port" 형식인지 검증한다. scrape 대상이
// pod IP 라는 전제를 dial 전에 강제해, 라벨 조작으로 임의 host 를 호출하는 표면을 차단한다.
func validAgentInstance(instance string) bool {
	host, port, err := net.SplitHostPort(instance)
	if err != nil {
		return false
	}
	if net.ParseIP(host) == nil {
		return false
	}
	for _, ch := range port {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return port != ""
}

// fetchAgentProcesses 는 agent 로컬 endpoint 를 호출해 스냅샷을 디코딩한다. 응답은 1MB 로 제한해
// (rca 프록시와 동일 규약) 비정상 응답의 메모리 표면을 막는다.
func (h *SynthesisHandler) fetchAgentProcesses(ctx context.Context, instance string) (*gpuobstypes.GPUProcessListing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+instance+"/processes", nil)
	if err != nil {
		return nil, err
	}
	res, err := h.agentClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	var listing gpuobstypes.GPUProcessListing
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&listing); err != nil {
		return nil, fmt.Errorf("응답 디코딩 실패: %w", err)
	}
	return &listing, nil
}

// buildGpuProcessesSummary 는 프로세스 수와 pod 귀속 수를 한 줄로 적는다.
func buildGpuProcessesSummary(r GpuProcessesResponse) string {
	if !r.Available {
		return fmt.Sprintf("노드 %s GPU 프로세스 조회 불가 (%s)", r.Node, r.Reason)
	}
	attributed := 0
	for _, p := range r.Processes {
		if p.Pod != "" {
			attributed++
		}
	}
	return fmt.Sprintf("노드 %s GPU 프로세스 %d개 (pod 귀속 %d)", r.Node, len(r.Processes), attributed)
}
