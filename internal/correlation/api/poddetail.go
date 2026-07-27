package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"netobs/internal/apicommon"
)

// pod-detail 은 #307 의 pod 단위 종합 상세 API 다. pod 상세 화면이 요구하는 단일 pod 의 종합 뷰
// (기본 정보와 health 3종, vitals, cgroup CPU throttle 상세, 네트워크 종합) 가 pods 와 trends 와
// flows 와 drops 에 분산되어 있던 것을 한 응답으로 합성한다. 서사 (narrative) 는 gpu-rca 와 rca 의
// 담당을 유지하고 본 응답은 수치 합성까지만 담는다.

// PodDetailResponse 는 GET /api/v1/pod/{namespace}/{pod} 의 typed 응답이다. 수집 공백 신호는
// 필드가 생략되도록 pointer (omitempty) 규약을 따른다.
type PodDetailResponse struct {
	GeneratedAt string `json:"generated_at"`
	Namespace   string `json:"namespace"`
	Pod         string `json:"pod"`
	// 기본 정보는 kube_pod_info 라벨에서 온다. pod 미관측 시 생략된다.
	UID           string `json:"uid,omitempty"`
	Node          string `json:"node,omitempty"`
	PodIP         string `json:"pod_ip,omitempty"`
	CreatedByKind string `json:"created_by_kind,omitempty"`
	CreatedByName string `json:"created_by_name,omitempty"`
	// Health 는 차원별 health (0-1, 1 이 healthy) 다. pod 단위 pressure score rule 의 1 - score
	// 환산이며, score 미산출 차원은 엔트리가 생략된다.
	Health  map[string]float64 `json:"health"`
	Vitals  PodVitals          `json:"vitals"`
	Cpu     PodCpuDetail       `json:"cpu"`
	Network PodNetworkDetail   `json:"network"`
	Summary string             `json:"summary"`
}

// PodVitals 는 사용률 스냅샷이다. CPU 와 memory 퍼센트는 limit 대비 비율이라 limit 미설정 pod 는
// 생략된다. CPUUsageCores 는 5분 rate 의 CPU 절대 사용량 (cores) 으로 limit 유무와 무관하게
// 산출되어 (#328), limit 없는 pod (CNI 와 kube-proxy 등) 도 memory 의 working set 절대값과 대칭으로
// CPU 수치를 갖는다.
type PodVitals struct {
	CPUPercent            *float64 `json:"cpu_percent,omitempty"`
	CPUUsageCores         *float64 `json:"cpu_usage_cores,omitempty"`
	MemoryPercent         *float64 `json:"memory_percent,omitempty"`
	MemoryWorkingSetBytes *float64 `json:"memory_working_set_bytes,omitempty"`
}

// PodCpuDetail 은 cgroup CFS throttle 상세다. quota 와 period (µs) 는 cadvisor 의
// container_spec_cpu_* 시리즈로, 노출하지 않는 클러스터에서는 생략된다. limit_cores 는
// kube_pod_container_resource_limits 기반으로 throttle 해석의 분모가 된다.
type PodCpuDetail struct {
	// ThrottledRatio 는 최근 5분 CFS period 중 throttle 된 비율 (0-1) 이다.
	ThrottledRatio     *float64 `json:"throttled_ratio,omitempty"`
	ThrottledPeriods5m *float64 `json:"throttled_periods_5m,omitempty"`
	TotalPeriods5m     *float64 `json:"total_periods_5m,omitempty"`
	LimitCores         *float64 `json:"limit_cores,omitempty"`
	QuotaMicroseconds  *float64 `json:"quota_microseconds,omitempty"`
	PeriodMicroseconds *float64 `json:"period_microseconds,omitempty"`
}

// PodNetworkDetail 은 netobs 의 pod 입도 신호 합성이다. 재전송과 drop 은 pod stage 이벤트
// (netobs_pod_stage_events_labeled_total 의 stage="retrans"/"drop") 의 rate 다. MaxSrttSeconds 는
// 커널 수집이 연결별 최대 smoothed RTT 라 분위수 (p50/p95/p99) 가 아님을 필드명으로 명시한다
// (latency-breakdown 과 동일 규약).
type PodNetworkDetail struct {
	RetransPerSec  *float64 `json:"retrans_per_sec,omitempty"`
	DropPerSec     *float64 `json:"drop_per_sec,omitempty"`
	MaxSrttSeconds *float64 `json:"max_srtt_seconds,omitempty"`
}

// parseNamespacePodPath 는 /api/v1/pod/ 뒤의 {namespace}/{pod} 두 세그먼트를 검증한다. 두 값 모두
// DNS-1123 형식 (parseNodeParam 과 동일 패턴) 을 PromQL %q 결합 전에 강제한다.
func parseNamespacePodPath(rest string) (string, string, error) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("경로는 /api/v1/pod/{namespace}/{pod} 형식이어야 합니다")
	}
	for _, p := range parts {
		if len(p) > 253 || !nodeNamePattern.MatchString(p) {
			return "", "", fmt.Errorf("namespace/pod 이름이 DNS-1123 형식이 아닙니다: %q", p)
		}
	}
	return parts[0], parts[1], nil
}

// GetPodDetail godoc
// @Summary      pod 단위 종합 상세
// @Description  단일 pod 의 기본 정보 (uid 와 node 와 pod_ip 와 owner) 와 차원별 health 3종 (pod 단위 pressure score 의 1 - score 환산), vitals (limit 대비 CPU 와 memory 사용률과 working set, CPU 절대 사용량), cgroup CPU throttle 상세 (throttled 비율과 5분 throttled/total periods 와 limit cores, cadvisor 가 노출하는 클러스터에서는 quota 와 period), 네트워크 종합 (pod 입도 재전송과 drop rate, 최대 smoothed RTT) 을 한 응답으로 합성한다. cpu_percent 는 limit 대비 비율이고 cpu_usage_cores 는 5분 rate 의 절대량 (cores) 이라 limit 없는 pod 는 cores 만 존재한다 (#328, memory 의 working set 절대값과 대칭). RTT 는 분위수가 아닌 최대값이라 필드명이 max_srtt_seconds 다. 미관측 pod 는 필드 생략과 summary 사유로 graceful 처리한다.
// @Tags         interference
// @Produce      json
// @Param        namespace  path   string  true   "pod 네임스페이스 (DNS-1123 형식)"
// @Param        pod        path   string  true   "pod 이름 (DNS-1123 형식)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  PodDetailResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/pod/{namespace}/{pod} [get]
func (h *SynthesisHandler) GetPodDetail(w http.ResponseWriter, r *http.Request) {
	ns, pod, err := parseNamespacePodPath(strings.TrimPrefix(r.URL.Path, "/api/v1/pod/"))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_pod_path", err.Error())
		return
	}
	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := PodDetailResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Namespace:   ns,
		Pod:         pod,
		Health:      map[string]float64{},
	}
	if h.querier == nil {
		resp.Summary = buildPodDetailSummary(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// ns 와 pod 는 parseNamespacePodPath 검증을 통과한 값이라 %q 결합이 안전하다. netobs 계열은
	// src_namespace/src_pod, kube/cadvisor 계열은 namespace/pod 라벨 규약을 쓴다.
	srcSel := fmt.Sprintf(`{src_namespace=%q, src_pod=%q}`, ns, pod)
	kubeSel := fmt.Sprintf(`{namespace=%q, pod=%q}`, ns, pod)
	// cadvisorSel 은 cadvisor 계열 sum 쿼리 전용이다. 표준 cadvisor 구성은 pod-level (container="")
	// 행과 container-level 행을 병존 노출해 무필터 sum 이 두 계층을 중복 합산하므로 pod-level 행으로
	// 한정한다 (node-vitals / node-resources 와 동일 규약). spec 계열 (quota / period) 은 pod-level
	// 행이 없는 구성에서 가드가 필드를 지우는 회귀가 되고 max 라 중복에 안전해 kubeSel 을 유지한다.
	cadvisorSel := fmt.Sprintf(`{namespace=%q, pod=%q, container=""}`, ns, pod)
	limitSel := func(resource string) string {
		return fmt.Sprintf(`{namespace=%q, pod=%q, resource=%q}`, ns, pod, resource)
	}

	res, qerr := h.queryParallel(ctx,
		"kube_pod_info"+kubeSel,
		"pod:cpu_throttle_score:5m"+srcSel,
		"pod:memory_pressure_score:5m"+srcSel,
		"pod:network_pressure_score:5m"+srcSel,
		fmt.Sprintf("sum(rate(container_cpu_usage_seconds_total%s[5m])) / sum(kube_pod_container_resource_limits%s) * 100", cadvisorSel, limitSel("cpu")),
		fmt.Sprintf("sum(container_memory_working_set_bytes%s) / sum(kube_pod_container_resource_limits%s) * 100", cadvisorSel, limitSel("memory")),
		"sum(container_memory_working_set_bytes"+cadvisorSel+")",
		fmt.Sprintf("sum(increase(container_cpu_cfs_throttled_periods_total%s[5m]))", cadvisorSel),
		fmt.Sprintf("sum(increase(container_cpu_cfs_periods_total%s[5m]))", cadvisorSel),
		"sum(kube_pod_container_resource_limits"+limitSel("cpu")+")",
		"max(container_spec_cpu_quota"+kubeSel+")",
		"max(container_spec_cpu_period"+kubeSel+")",
		fmt.Sprintf(`sum(rate(netobs_pod_stage_events_labeled_total{stage="retrans", src_namespace=%q, src_pod=%q}[5m]))`, ns, pod),
		fmt.Sprintf(`sum(rate(netobs_pod_stage_events_labeled_total{stage="drop", src_namespace=%q, src_pod=%q}[5m]))`, ns, pod),
		"max(netobs_tcp_state_max_srtt_seconds"+kubeSel+")",
		// #328 CPU 절대 사용량 (cores). limit 분모 percent 와 달리 limit 유무와 무관하게 산출된다.
		fmt.Sprintf("sum(rate(container_cpu_usage_seconds_total%s[5m]))", cadvisorSel),
	)
	if qerr != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", qerr))
		return
	}

	if len(res[0]) > 0 {
		l := res[0][0].Labels
		resp.UID = l["uid"]
		resp.Node = l["node"]
		resp.PodIP = l["pod_ip"]
		resp.CreatedByKind = l["created_by_kind"]
		resp.CreatedByName = l["created_by_name"]
	}
	// health = 1 - score (0-1 clamp). score 미산출 차원은 엔트리 생략.
	for i, dim := range []string{"cpu", "memory", "network"} {
		if v := firstValue(res[1+i]); v != nil {
			s := *v
			if s < 0 {
				s = 0
			}
			if s > 1 {
				s = 1
			}
			resp.Health[dim] = 1 - s
		}
	}
	resp.Vitals.CPUPercent = firstValue(res[4])
	resp.Vitals.MemoryPercent = firstValue(res[5])
	resp.Vitals.MemoryWorkingSetBytes = firstValue(res[6])
	resp.Cpu.ThrottledPeriods5m = firstValue(res[7])
	resp.Cpu.TotalPeriods5m = firstValue(res[8])
	// throttled 비율은 이미 조회한 두 카운터에서 파생 계산한다 (nodevitals 의 GPUMemoryPercent
	// 관용구). 분모 0 (5분간 period 없음) 이면 생략된다.
	if resp.Cpu.ThrottledPeriods5m != nil && resp.Cpu.TotalPeriods5m != nil && *resp.Cpu.TotalPeriods5m > 0 {
		ratio := *resp.Cpu.ThrottledPeriods5m / *resp.Cpu.TotalPeriods5m
		resp.Cpu.ThrottledRatio = &ratio
	}
	resp.Cpu.LimitCores = firstValue(res[9])
	resp.Cpu.QuotaMicroseconds = firstValue(res[10])
	resp.Cpu.PeriodMicroseconds = firstValue(res[11])
	resp.Network.RetransPerSec = firstValue(res[12])
	resp.Network.DropPerSec = firstValue(res[13])
	resp.Network.MaxSrttSeconds = firstValue(res[14])
	resp.Vitals.CPUUsageCores = firstValue(res[15])

	resp.Summary = buildPodDetailSummary(resp)
	apicommon.WriteJSON(w, resp)
}

// buildPodDetailSummary 는 pod 종합 상태를 한 줄로 요약한다. 미관측 pod (kube_pod_info 부재) 는
// 사유를 적는다.
func buildPodDetailSummary(r PodDetailResponse) string {
	if r.Node == "" {
		return fmt.Sprintf("pod %s/%s 의 관측 데이터가 없습니다 (미존재 또는 미수집)", r.Namespace, r.Pod)
	}
	seg := fmt.Sprintf("pod %s/%s (노드 %s)", r.Namespace, r.Pod, r.Node)
	for _, dim := range []string{"cpu", "memory", "network"} {
		if v, ok := r.Health[dim]; ok {
			seg += fmt.Sprintf(", %s health %.2f", dim, v)
		}
	}
	if r.Cpu.ThrottledRatio != nil {
		seg += fmt.Sprintf(", throttle %.0f%%", *r.Cpu.ThrottledRatio*100)
	}
	if r.Network.RetransPerSec != nil {
		seg += fmt.Sprintf(", 재전송 %.1f/s", *r.Network.RetransPerSec)
	}
	return seg
}
