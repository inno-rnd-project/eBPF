package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// gpu-rca 는 #258 의 노드 단위 GPU RCA 합성 엔드포인트다. "노드 하나의 GPU 가 왜 노는가" 를 dominant
// cause 와 cause 별 가중치, 신뢰도, 원인 후보 pod 랭킹, 한 줄 narrative 로 한 응답에 합성한다. 개별
// 지표 (raw 사용률, 온도, 전력, 시계열) 는 Grafana 가 Prometheus 에서 직접 읽으므로 본 응답은 단일
// PromQL 로 표현 불가능한 cross-signal 합성에만 한정한다.

// NodeGpuRcaResponse 는 GET /api/v1/gpu-rca 의 typed 응답이다.
type NodeGpuRcaResponse struct {
	GeneratedAt string `json:"generated_at"`
	Node        string `json:"node"`
	// Gpu 는 device 스코프 조회 시 요청 gpu 파라미터 (GPU UUID 또는 device index) 의 echo 다.
	Gpu string `json:"gpu,omitempty"`
	// Idle 은 node:gpu_idle:5m (0-1). 유휴 게이팅 미충족 시 cause 귀속이 비므로 함께 노출한다.
	Idle          float64 `json:"idle"`
	DominantCause string  `json:"dominant_cause,omitempty"`
	// Confidence 는 dominant 판정 신뢰도 (0-1). top1 과 top2 cause weight 의 격차 (margin) 로,
	// 단일 cause 가 지배적일수록 1 에 가깝고 두 cause 가 백중이면 0 에 가깝다. GPUIdleDominantCause
	// Ambiguous alert 의 격차 < 0.1 판정과 동일 축이다.
	Confidence float64          `json:"confidence"`
	Causes     []GpuCauseWeight `json:"causes"`
	Suspects   []RcaSuspect     `json:"suspects"`
	// Evidence 는 narrative 를 뒷받침하는 근거 수치다.
	Evidence  RcaEvidence `json:"evidence"`
	Narrative string      `json:"narrative"`
}

// RcaEvidence 는 dominant cause 판정을 뒷받침하는 instant 근거 수치다. 수집 공백 시 필드가 생략된다.
// GPU 수치는 gpu 파라미터가 있으면 그 device 로 좁혀지고, 없으면 노드 device 평균이다.
type RcaEvidence struct {
	// GpuUtilizationPercent 는 device 사용률 (0-100) 이다. GPM 미지원 GPU 의 SM active fallback 축.
	GpuUtilizationPercent *float64 `json:"gpu_utilization_percent,omitempty"`
	// SMActivePercent 는 gpm sm_occupancy 로, 데이터센터 GPU 에서만 채워진다.
	SMActivePercent *float64 `json:"sm_active_percent,omitempty"`
	// RetransPerSec 는 이 노드의 TCP 재전송 rate (5m) 다.
	RetransPerSec *float64 `json:"retrans_per_sec,omitempty"`
	// MaxSrttSeconds 는 이 노드 연결들의 최대 smoothed RTT 다. latency-breakdown 과 동일 소스
	// (netobs_tcp_state_max_srtt_seconds) 로, p99 가 아닌 최대값이라 필드명도 max 로 적는다.
	MaxSrttSeconds *float64 `json:"max_srtt_seconds,omitempty"`

	// 아래는 #287 의 dominant cause 차원 맞춤 수치로, dominant 판정 후 2차 instant 조회로 채워져
	// 해당 cause 일 때만 존재한다.

	// CpuThrottleRatio 는 dominant 가 cpu_throttle 일 때 이 노드 pod 의 최대 throttle 비율 (0-1) 이다.
	CpuThrottleRatio *float64 `json:"cpu_throttle_ratio,omitempty"`
	// SuspectMemoryLimitRatio 는 dominant 가 memory_pressure 일 때 최우선 suspect pod 의
	// working_set/limit 비율 (0-1) 이다. suspect 가 없으면 노드 pod 최대값으로 fallback 한다.
	SuspectMemoryLimitRatio *float64 `json:"suspect_memory_limit_ratio,omitempty"`
	// NodeMemoryUsedRatio 는 dominant 가 memory_pressure 일 때 노드 실측 메모리 사용률 (0-1) 이다.
	NodeMemoryUsedRatio *float64 `json:"node_memory_used_ratio,omitempty"`
	// TemperatureCelsius 와 SlowdownHeadroomCelsius 는 dominant 가 thermal 일 때 노드 device 최고
	// 온도와 slowdown 임계까지의 여유다. 여유는 임계와 온도가 모두 수집됐을 때만 산출된다.
	TemperatureCelsius      *float64 `json:"temperature_celsius,omitempty"`
	SlowdownHeadroomCelsius *float64 `json:"slowdown_headroom_celsius,omitempty"`
	// CauseScore 는 점수형 cause (pcie_saturation, dcgm_pcie_replay, nccl_collective_stall,
	// host_compute_stall, cgroup_contention) 의 노드 score (0-1) 다.
	CauseScore *float64 `json:"cause_score,omitempty"`

	// slowdownThresholdCelsius 는 여유 산출용 중간값으로 응답에는 싣지 않는다.
	slowdownThresholdCelsius *float64
}

// finalizeEvidence 는 2차 조회 완료 후 파생 수치를 계산한다.
func (e *RcaEvidence) finalizeEvidence() {
	if e.TemperatureCelsius != nil && e.slowdownThresholdCelsius != nil {
		h := *e.slowdownThresholdCelsius - *e.TemperatureCelsius
		e.SlowdownHeadroomCelsius = &h
	}
}

// rcaEvidenceBind 는 dominant cause 일 때만 실행하는 차원 맞춤 instant 조회 한 건이다. query 가 빈
// 문자열을 돌려주면 그 바인딩은 건너뛴다.
type rcaEvidenceBind struct {
	// query 는 node 와 device 스코프 matcher (gpu 파라미터 유래, 없으면 빈 문자열), 최우선 suspect
	// 로 instant 쿼리를 만든다. device 차원이 없는 메트릭은 matcher 를 무시한다.
	query func(node, gpuMatcher string, top *RcaSuspect) string
	set   func(e *RcaEvidence, v float64)
}

// rcaCauseInfo 는 cause 한 종의 서사 요소다. description 은 causes[].description 과 narrative 의
// slug 부연으로, chain 은 dominant 일 때 narrative 에 덧붙는 인과 체인 문구로 쓰인다.
type rcaCauseInfo struct {
	description string
	chain       string
	evidence    []rcaEvidenceBind
}

// nodeScoreBind 는 node 스코프 단일 시리즈를 CauseScore 로 싣는 공용 바인딩이다. metric 은 고정
// 리터럴이고 node 는 parseNodeParam 검증을 통과한 값이라 %q 결합이 안전하다.
func nodeScoreBind(metric string) []rcaEvidenceBind {
	return []rcaEvidenceBind{{
		query: func(node, _ string, _ *RcaSuspect) string {
			return fmt.Sprintf("max(%s{node=%q})", metric, node)
		},
		set: func(e *RcaEvidence, v float64) { e.CauseScore = &v },
	}}
}

// rcaCauseCatalog 는 #287 의 cause 레지스트리다. playbookCatalog 와 동일하게 항목 추가 시 본
// 카탈로그를 갱신한다. cause enum 은 gpu_idle_cause_weight:5m 의 9종과 동일하다. suspect 식별자는
// noisy-neighbor snapshot (kube informer 유래) 값이라 %q 결합이 안전하다.
var rcaCauseCatalog = map[string]rcaCauseInfo{
	"cpu_throttle": {
		description: "host 스레드의 CFS quota throttle 로 GPU 공급 정체",
		chain:       "CPU throttle → 공급 스레드 지연 → GPU 대기",
		evidence: []rcaEvidenceBind{{
			query: func(node, _ string, _ *RcaSuspect) string {
				return fmt.Sprintf("max(pod:cpu_throttle_score:5m{node=%q})", node)
			},
			set: func(e *RcaEvidence, v float64) { e.CpuThrottleRatio = &v },
		}},
	},
	"memory_pressure": {
		description: "working set 의 memory limit 근접으로 allocation stall",
		chain:       "메모리 reclaim/stall → 파이프라인 블로킹 → GPU 대기",
		evidence: []rcaEvidenceBind{
			{
				query: func(node, _ string, top *RcaSuspect) string {
					if top != nil && top.Pod != "" {
						return fmt.Sprintf("max(pod:memory_pressure_score:5m{node=%q, src_namespace=%q, src_pod=%q})", node, top.Namespace, top.Pod)
					}
					return fmt.Sprintf("max(pod:memory_pressure_score:5m{node=%q})", node)
				},
				set: func(e *RcaEvidence, v float64) { e.SuspectMemoryLimitRatio = &v },
			},
			{
				query: func(node, _ string, _ *RcaSuspect) string {
					return fmt.Sprintf("node:memory_pressure_score:5m{node=%q}", node)
				},
				set: func(e *RcaEvidence, v float64) { e.NodeMemoryUsedRatio = &v },
			},
		},
	},
	"network_pressure": {
		description: "throughput 포화 또는 재전송 급증으로 데이터 대기",
		chain:       "재전송 → 통신 블로킹 → GPU 대기",
		// network 수치는 1차 조회의 재전송 rate 와 최대 RTT 가 담당한다.
	},
	"nccl_collective_stall": {
		description: "collective 연산의 rank 간 동기화 대기",
		chain:       "collective 동기화 대기 → rank 정체 → GPU 대기",
		evidence:    nodeScoreBind("node:nccl_collective_stall_score:5m"),
	},
	"pcie_saturation": {
		description: "host 와 GPU 간 PCIe 전송 포화",
		chain:       "PCIe 복사 정체 → 데이터 공급 지연 → GPU 대기",
		evidence:    nodeScoreBind("node:gpu_pcie_saturation_score:5m"),
	},
	"host_compute_stall": {
		description: "kernel launch 부족 또는 device memory 포화",
		chain:       "host 연산 병목 → 커널 공급 부족 → GPU 대기",
		evidence:    nodeScoreBind("pod:host_compute_stall_score:5m"),
	},
	"dcgm_pcie_replay": {
		description: "PCIe 전송 오류 재시도 증가 (하드웨어 징후)",
		chain:       "PCIe replay 재시도 → 전송 지연 → GPU 대기",
		evidence:    nodeScoreBind("node:dcgm_pcie_replay_score:5m"),
	},
	"thermal": {
		description: "온도 임계 도달로 클럭 강제 하향",
		chain:       "고온 → 클럭 하향 → 실효 성능 저하",
		evidence: []rcaEvidenceBind{
			{
				query: func(node, gpuMatcher string, _ *RcaSuspect) string {
					return "max(gpuobs_device_temperature_celsius" + promSelector(nodeMatcher(node), gpuMatcher) + ")"
				},
				set: func(e *RcaEvidence, v float64) { e.TemperatureCelsius = &v },
			},
			{
				query: func(node, gpuMatcher string, _ *RcaSuspect) string {
					return "min(gpuobs_device_temperature_threshold_celsius" + promSelector(nodeMatcher(node), gpuMatcher, `threshold="slowdown"`) + ")"
				},
				set: func(e *RcaEvidence, v float64) { e.slowdownThresholdCelsius = &v },
			},
		},
	},
	"cgroup_contention": {
		description: "이웃 cgroup 과의 CPU 시간 경합 (PSI stall)",
		chain:       "CPU 경합 stall → 공급 스레드 지연 → GPU 대기",
		evidence:    nodeScoreBind("pod:cgroup_contention_score:5m"),
	},
}

// gpuParamPattern 은 gpu 쿼리 파라미터 (NVIDIA GPU/MIG UUID 또는 device index) 의 문자 구성이다.
// UUID 는 영숫자와 하이픈 (GPU-8f6f... 형태) 이라 parseNodeParam 과 동일하게 exact = 매처와 %q 결합
// 전제 (PromQL 메타문자 부재) 를 입력 경계에서 보장한다.
var gpuParamPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,96}$`)

// gpuIndexPattern 은 십진 숫자만의 gpu 값이다. 이 형태는 gpu_index 라벨로 매칭한다.
var gpuIndexPattern = regexp.MustCompile(`^[0-9]+$`)

// parseGpuParam 은 gpu 파라미터를 검증해 PromQL exact matcher 조각으로 만든다. 십진 숫자만이면
// gpu_index, 그 외는 gpu_uuid 매처다. 빈 값은 "device 스코프 없음" 이라 빈 문자열을 돌려준다.
func parseGpuParam(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if !gpuParamPattern.MatchString(raw) {
		return "", fmt.Errorf("gpu 는 GPU UUID 또는 device index 형식이어야 합니다: %q", raw)
	}
	if gpuIndexPattern.MatchString(raw) {
		return fmt.Sprintf("gpu_index=%q", raw), nil
	}
	return fmt.Sprintf("gpu_uuid=%q", raw), nil
}

// RcaSuspect 는 이 노드의 GPU 유휴에 기여하는 원인 후보다. noisy-neighbor 는 이 노드 pod 를 victim
// 으로 하는 suspect pod (namespace-aware) 이고, cross-node 는 이 노드를 victim_node 로 하는 suspect
// node 다. cross-node 후보는 pod 식별자가 없어 namespace/pod 가 비고 node 만 채워진다.
type RcaSuspect struct {
	Source    string   `json:"source"`
	Namespace string   `json:"namespace,omitempty"`
	Pod       string   `json:"pod,omitempty"`
	Node      string   `json:"node,omitempty"`
	Dimension string   `json:"dimension"`
	Score     float64  `json:"score"`
	Issues    []string `json:"issues,omitempty"`
}

// GetNodeGpuRca godoc
// @Summary      노드 GPU RCA 합성
// @Description  노드 하나의 GPU 유휴 dominant cause 와 cause 별 가중치 (한국어 설명 포함), 신뢰도, 원인 후보 pod 랭킹, 근거 수치 (evidence), 한 줄 narrative 를 한 응답으로 합성한다. dominant cause 와 가중치는 scope=node gpu-idle 결과를 재사용하고, 원인 후보는 이 노드를 victim 으로 하는 noisy-neighbor suspect pod (namespace-aware) 와 cross-node-interference suspect node 를 점수순으로 집계한다. 신뢰도는 top1 과 top2 cause 격차이며 0.1 미만 백중세는 narrative 에 판정 유보로 적는다. evidence 는 device 사용률과 SM active, 노드 재전송 rate 와 최대 RTT 에 더해 dominant cause 의 차원 맞춤 수치 (memory 는 suspect pod 의 working_set/limit 과 노드 실측 사용률, thermal 은 온도와 slowdown 여유, cpu 는 throttle 비율, 점수형 cause 는 노드 score) 를 cause 레지스트리 기반 2차 조회로 실어 narrative 에 융합하고, dominant cause 별 인과 체인 문구를 덧붙인다. gpu 파라미터 (GPU UUID 또는 device index) 로 evidence 의 GPU 수치를 device 로 좁힐 수 있고, 미등록 device 는 해당 수치가 생략된다.
// @Tags         gpu
// @Produce      json
// @Param        node   query  string  true   "대상 노드 (DNS-1123 형식)"
// @Param        gpu    query  string  false  "대상 device (GPU UUID 또는 device index, 생략 시 노드 전체)"
// @Param        limit  query  int     false  "원인 후보 상위 N (1-50, 기본 10)"
// @Param        at     query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  NodeGpuRcaResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Router       /api/v1/gpu-rca [get]
func (h *SynthesisHandler) GetNodeGpuRca(w http.ResponseWriter, r *http.Request) {
	node, err := parseNodeParam(strings.TrimSpace(r.URL.Query().Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}
	if node == "" {
		apicommon.WriteError(w, http.StatusBadRequest, "missing_node", "node 파라미터가 필요합니다")
		return
	}
	gpu := strings.TrimSpace(r.URL.Query().Get("gpu"))
	gpuMatcher, err := parseGpuParam(gpu)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_gpu", err.Error())
		return
	}
	limit := 10
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			limit = n
		}
	}
	if limit > 50 {
		limit = 50
	}

	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := NodeGpuRcaResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Node:        node,
		Gpu:         gpu,
		Causes:      []GpuCauseWeight{},
		Suspects:    []RcaSuspect{},
	}
	if h.querier == nil {
		resp.Narrative = buildRcaNarrative(resp)
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// node 는 parseNodeParam, gpu 는 parseGpuParam 검증을 통과한 값이라 exact = 매처와 %q 결합이
	// 안전하다. devSel 은 evidence 의 GPU 수치용 device selector 로, gpu 가 비면 노드 전체다.
	sel := fmt.Sprintf("{node=%q}", node)
	devSel := promSelector(nodeMatcher(node), gpuMatcher)
	gpmSel := promSelector(nodeMatcher(node), gpuMatcher, `gpm="sm_occupancy"`)

	// 쿼리를 상수로 고정해 병렬 실행 (idle / cause weight / dominant / pod-node 매핑 / evidence). ALERTS
	// 는 node selector 를 붙이지 않아 (alert 라벨 규약이 node 마다 달라) 별도로 조회한다. snapshot
	// (neighbors / crossNode) 은 in-memory 라 쿼리에 포함되지 않는다. 미등록 device 는 avg 결과가 비어
	// firstValue 가 nil 을 돌려주고 evidence 필드가 생략된다 (graceful).
	res := h.queryParallel(ctx,
		"node:gpu_idle:5m"+sel,
		"node:gpu_idle_cause_weight:5m"+sel,
		"node:gpu_idle_dominant_cause:5m"+sel,
		"kube_pod_info"+sel,
		`ALERTS{alertstate="firing"}`,
		"avg(gpuobs_device_utilization_percent"+devSel+")",
		"avg(gpuobs_device_gpm_utilization_percent"+gpmSel+")",
		"sum(rate(netobs_retrans_events_labeled_total"+sel+"[5m]))",
		"max(netobs_tcp_state_max_srtt_seconds"+sel+")",
	)
	firing := res[4]
	resp.Evidence.GpuUtilizationPercent = firstValue(res[5])
	resp.Evidence.SMActivePercent = firstValue(res[6])
	resp.Evidence.RetransPerSec = firstValue(res[7])
	resp.Evidence.MaxSrttSeconds = firstValue(res[8])

	if len(res[0]) > 0 && !math.IsNaN(res[0][0].Value) {
		resp.Idle = res[0][0].Value
	}

	// dominant cause 와 cause 가중치. gpu-idle scope=node 와 동일 소스라 해석이 일치한다.
	for _, sm := range res[1] {
		if math.IsNaN(sm.Value) {
			continue
		}
		if c := sm.Labels["cause"]; c != "" {
			resp.Causes = append(resp.Causes, GpuCauseWeight{Cause: c, Weight: sm.Value})
		}
	}
	sortCauses(resp.Causes)
	if len(resp.Causes) > 0 {
		resp.DominantCause = resp.Causes[0].Cause
	}
	for _, sm := range res[2] {
		if c := sm.Labels["cause"]; c != "" {
			resp.DominantCause = c
		}
	}
	resp.Confidence = dominantConfidence(resp.Causes)

	// 이 노드에 사는 pod 의 (namespace, pod) 집합. noisy-neighbor victim 이 이 노드인지 판정하는
	// 근거이며, 동명 pod 오귀속을 막기 위해 namespace 까지 함께 키로 쓴다.
	nodePods := map[[2]string]bool{}
	for _, sm := range res[3] {
		if ns, pod := sm.Labels["namespace"], sm.Labels["pod"]; pod != "" {
			nodePods[[2]string{ns, pod}] = true
		}
	}

	resp.Suspects = h.rcaSuspects(node, nodePods, firing, limit)

	// #287 cause 레지스트리. causes 에 한국어 설명을 채우고, dominant cause 의 차원 맞춤 evidence
	// 를 2차 instant 조회로 싣는다.
	for i := range resp.Causes {
		info := rcaCauseCatalog[resp.Causes[i].Cause]
		resp.Causes[i].Description = info.description
		resp.Causes[i].Chain = info.chain
	}
	h.fetchCauseEvidence(ctx, node, gpuMatcher, &resp)

	resp.Narrative = buildRcaNarrative(resp)
	apicommon.WriteJSON(w, resp)
}

// fetchCauseEvidence 는 dominant cause 의 레지스트리 evidence 바인딩을 실행해 차원 맞춤 수치를
// 채운다. dominant 가 없거나 (유휴 게이팅 미충족) 바인딩이 없는 cause 면 아무것도 하지 않는다.
// suspect 스코프 바인딩은 최우선 suspect 를 받으며, 실패 (nil) 는 필드 생략으로 graceful 처리한다.
func (h *SynthesisHandler) fetchCauseEvidence(ctx context.Context, node, gpuMatcher string, resp *NodeGpuRcaResponse) {
	info, ok := rcaCauseCatalog[resp.DominantCause]
	if !ok || len(info.evidence) == 0 {
		return
	}
	var top *RcaSuspect
	if len(resp.Suspects) > 0 {
		top = &resp.Suspects[0]
	}
	queries := make([]string, 0, len(info.evidence))
	sets := make([]func(*RcaEvidence, float64), 0, len(info.evidence))
	for _, b := range info.evidence {
		q := b.query(node, gpuMatcher, top)
		if q == "" {
			continue
		}
		queries = append(queries, q)
		sets = append(sets, b.set)
	}
	if len(queries) == 0 {
		return
	}
	res := h.queryParallel(ctx, queries...)
	for i, samples := range res {
		if v := firstValue(samples); v != nil {
			sets[i](&resp.Evidence, *v)
		}
	}
	resp.Evidence.finalizeEvidence()
}

// dominantConfidence 는 top1 과 top2 cause weight 의 격차를 신뢰도로 돌려준다. cause 가 없으면 0,
// 단일 cause 면 그 weight, 둘 이상이면 margin (top1 - top2) 이다. causes 는 호출 전 sortCauses 로
// weight 내림차순 정렬돼 있어 [0] 이 top1, [1] 이 top2 다.
func dominantConfidence(causes []GpuCauseWeight) float64 {
	if len(causes) == 0 {
		return 0
	}
	top := causes[0].Weight
	second := 0.0
	if len(causes) >= 2 {
		second = causes[1].Weight
	}
	margin := top - second
	if margin < 0 {
		margin = 0
	}
	if margin > 1 {
		margin = 1
	}
	return margin
}

// rcaSuspects 는 이 노드의 원인 후보를 noisy-neighbor 와 cross-node snapshot 에서 집계해 score
// 내림차순으로 돌려준다. noisy-neighbor 는 victim (namespace, pod) 이 이 노드에 사는 경우의 suspect
// pod 이고, cross-node 는 victim_node 가 이 노드인 경우의 suspect node 다. 동일 suspect 가 victim
// 신호나 dimension 별로 여러 페어를 갖는 실측 케이스가 흔해, (source, namespace, pod, node) 키로
// 최고 score 만 남겨 후보당 1 건의 랭킹으로 집계한다. suspect pod 에는 namespace-aware alert 매칭
// 으로 firing alertname 을 붙인다.
func (h *SynthesisHandler) rcaSuspects(node string, nodePods map[[2]string]bool, firing []correlation.InstantSample, limit int) []RcaSuspect {
	type key struct{ source, ns, pod, node string }
	best := map[key]RcaSuspect{}
	order := []key{}
	upsert := func(k key, cand RcaSuspect) {
		if cur, ok := best[k]; ok {
			if cand.Score > cur.Score {
				best[k] = cand
			}
			return
		}
		best[k] = cand
		order = append(order, k)
	}
	if h.neighbors != nil {
		for _, n := range h.neighbors.Snapshot() {
			if !nodePods[[2]string{n.Victim.Namespace, n.Victim.Pod}] {
				continue
			}
			k := key{"noisy_neighbor", n.Suspect.Namespace, n.Suspect.Pod, ""}
			upsert(k, RcaSuspect{
				Source:    "noisy_neighbor",
				Namespace: n.Suspect.Namespace,
				Pod:       n.Suspect.Pod,
				Dimension: string(n.Dimension),
				Score:     n.Score,
				Issues:    podIssues(firing, n.Suspect.Namespace, n.Suspect.Pod),
			})
		}
	}
	if h.crossNode != nil {
		for _, ni := range h.crossNode.CrossNodeSnapshot() {
			if ni.VictimNode != node {
				continue
			}
			k := key{"cross_node", "", "", ni.SuspectNode}
			upsert(k, RcaSuspect{
				Source:    "cross_node",
				Node:      ni.SuspectNode,
				Dimension: string(ni.Dimension),
				Score:     ni.Score,
			})
		}
	}
	out := make([]RcaSuspect, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	// score 내림차순, 동률은 source·식별자 사전순으로 결정성을 확보한다.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].Node < out[j].Node
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// rcaAmbiguousMargin 은 top1 과 top2 cause 가 백중이라 판정을 유보하는 신뢰도 임계다.
// GPUIdleDominantCauseAmbiguous alert 의 격차 < 0.1 판정과 동일 축이다.
const rcaAmbiguousMargin = 0.1

// buildRcaNarrative 는 dominant cause 와 신뢰도, 최우선 의심 후보, 근거 수치를 한 줄로 합성한다
// (#287 레지스트리 융합). gpu 파라미터가 있으면 주어를 device 로 좁혀 적고, dominant cause 에는
// 레지스트리의 한국어 설명 부연과 인과 체인 문구를 덧붙인다. top1 과 top2 가 백중 (margin < 0.1)
// 이면 판정 유보를 함께 적는다.
func buildRcaNarrative(r NodeGpuRcaResponse) string {
	subject := "노드 " + r.Node
	if r.Gpu != "" {
		subject += " device " + r.Gpu
	}
	if r.DominantCause == "" {
		// dominant 부재는 두 상황이다. 유휴 게이팅 미충족 (idle <= 0.5) 과, 유휴지만 baseline 대비
		// 신규 압박 rise 가 없어 cause weight 가 비는 정상 상태 (#285 이후 평시 형태).
		out := fmt.Sprintf("%s GPU 유휴 원인 귀속 임계(idle>0.5) 미만 (idle %.2f)", subject, r.Idle)
		if r.Idle > 0.5 {
			out = fmt.Sprintf("%s GPU 유휴 (idle %.2f) 이나 baseline 대비 신규 압박 rise 가 없어 귀속할 cause 없음", subject, r.Idle)
		}
		if ev := rcaEvidenceText(r.Evidence); ev != "" {
			out += ", 근거 " + ev
		}
		return out
	}
	info := rcaCauseCatalog[r.DominantCause]
	dominant := r.DominantCause
	if info.description != "" {
		dominant += "(" + info.description + ")"
	}
	out := fmt.Sprintf("%s GPU 유휴 dominant 원인 %s (신뢰도 %.2f, idle %.2f)", subject, dominant, r.Confidence, r.Idle)
	if r.Confidence < rcaAmbiguousMargin && len(r.Causes) >= 2 {
		out += fmt.Sprintf(", top2 %s 와 백중이라 판정 유보", r.Causes[1].Cause)
	}
	if len(r.Suspects) > 0 {
		s := r.Suspects[0]
		who := s.Node
		if s.Pod != "" {
			who = s.Namespace + "/" + s.Pod
		}
		out += fmt.Sprintf(", 최우선 의심 %s (%s, score %.2f)", who, s.Dimension, s.Score)
	}
	if ev := rcaEvidenceText(r.Evidence); ev != "" {
		out += ", 근거 " + ev
	}
	if info.chain != "" {
		out += " (인과 체인: " + info.chain + ")"
	}
	return out
}

// rcaEvidenceText 는 evidence 수치를 narrative 조각으로 합성한다. GPU 축은 SM active 를 우선하고
// GPM 미지원 GPU 는 device 사용률로 fallback 한다. GPU 축과 network 축이 함께 있으면 "인데" 로 이어
// 낮은 연산 점유와 network 수치의 대비를 드러낸다. 전 필드 공백이면 빈 문자열이다.
func rcaEvidenceText(e RcaEvidence) string {
	gpuPart := ""
	switch {
	case e.SMActivePercent != nil:
		gpuPart = fmt.Sprintf("SM active %.1f%%", *e.SMActivePercent)
	case e.GpuUtilizationPercent != nil:
		gpuPart = fmt.Sprintf("GPU 사용률 %.1f%%", *e.GpuUtilizationPercent)
	}
	netSegs := []string{}
	if e.RetransPerSec != nil {
		netSegs = append(netSegs, fmt.Sprintf("재전송 %.1f/s", *e.RetransPerSec))
	}
	if e.MaxSrttSeconds != nil {
		netSegs = append(netSegs, fmt.Sprintf("RTT %.0fms", *e.MaxSrttSeconds*1000))
	}
	netPart := strings.Join(netSegs, "·")
	base := ""
	switch {
	case gpuPart != "" && netPart != "":
		base = gpuPart + "인데 " + netPart
	case gpuPart != "":
		base = gpuPart
	default:
		base = netPart
	}
	// #287 dominant cause 차원 맞춤 수치. 2차 조회로 채워진 필드만 존재하므로 dominant 와 무관한
	// 조각이 섞이지 않는다.
	causeSegs := []string{}
	if e.CpuThrottleRatio != nil {
		causeSegs = append(causeSegs, fmt.Sprintf("CPU throttle 비율 %.0f%%", *e.CpuThrottleRatio*100))
	}
	if e.SuspectMemoryLimitRatio != nil {
		causeSegs = append(causeSegs, fmt.Sprintf("의심 pod 메모리 working_set/limit %.0f%%", *e.SuspectMemoryLimitRatio*100))
	}
	if e.NodeMemoryUsedRatio != nil {
		causeSegs = append(causeSegs, fmt.Sprintf("노드 메모리 사용률 %.0f%%", *e.NodeMemoryUsedRatio*100))
	}
	if e.TemperatureCelsius != nil {
		seg := fmt.Sprintf("온도 %.0f°C", *e.TemperatureCelsius)
		if e.SlowdownHeadroomCelsius != nil {
			seg += fmt.Sprintf("(slowdown 여유 %.0f°C)", *e.SlowdownHeadroomCelsius)
		}
		causeSegs = append(causeSegs, seg)
	}
	if e.CauseScore != nil {
		causeSegs = append(causeSegs, fmt.Sprintf("cause score %.2f", *e.CauseScore))
	}
	causePart := strings.Join(causeSegs, "·")
	switch {
	case base != "" && causePart != "":
		return base + "·" + causePart
	case causePart != "":
		return causePart
	default:
		return base
	}
}
