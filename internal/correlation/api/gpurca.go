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
// @Description  노드 하나의 GPU 유휴 dominant cause 와 cause 별 가중치, 신뢰도, 원인 후보 pod 랭킹, 근거 수치 (evidence), 한 줄 narrative 를 한 응답으로 합성한다. dominant cause 와 가중치는 scope=node gpu-idle 결과를 재사용하고, 원인 후보는 이 노드를 victim 으로 하는 noisy-neighbor suspect pod (namespace-aware) 와 cross-node-interference suspect node 를 점수순으로 집계한다. 신뢰도는 top1 과 top2 cause 격차다. gpu 파라미터 (GPU UUID 또는 device index) 로 evidence 의 GPU 수치를 device 로 좁힐 수 있고, 미등록 device 는 해당 수치가 생략된다.
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
	)
	firing := res[4]
	resp.Evidence.GpuUtilizationPercent = firstValue(res[5])
	resp.Evidence.SMActivePercent = firstValue(res[6])

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
	resp.Narrative = buildRcaNarrative(resp)
	apicommon.WriteJSON(w, resp)
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

// buildRcaNarrative 는 dominant cause 와 신뢰도, 최우선 의심 후보를 한 줄로 합성한다. gpu 파라미터가
// 있으면 주어를 device 로 좁혀 적는다.
func buildRcaNarrative(r NodeGpuRcaResponse) string {
	subject := "노드 " + r.Node
	if r.Gpu != "" {
		subject += " device " + r.Gpu
	}
	if r.DominantCause == "" {
		return fmt.Sprintf("%s GPU 유휴 원인 귀속 임계(idle>0.5) 미만 (idle %.2f)", subject, r.Idle)
	}
	base := fmt.Sprintf("%s GPU 유휴 dominant 원인 %s (신뢰도 %.2f, idle %.2f)", subject, r.DominantCause, r.Confidence, r.Idle)
	if len(r.Suspects) == 0 {
		return base
	}
	s := r.Suspects[0]
	who := s.Node
	if s.Pod != "" {
		who = s.Namespace + "/" + s.Pod
	}
	return fmt.Sprintf("%s, 최우선 의심 %s (%s, score %.2f)", base, who, s.Dimension, s.Score)
}
