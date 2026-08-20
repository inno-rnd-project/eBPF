package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"netobs/internal/apicommon"
)

// BandwidthResponse 는 GET /api/v1/bandwidth 의 typed 응답이다. flows 가 allow-list 게이팅 5-tuple
// 상세를 다루는 것과 별개로, allow-list 없이 상시 수집되는 netobs_pod_bytes_total 의 rate 로 전체
// pod 의 RX/TX 대역폭 (bytes/sec) 을 노출한다. BPF 는 (egress, l4) / (egress, nic) / (ingress, l4)
// 세 조합만 누적하므로 RX 는 l4 관점만 존재하고 NIC 관점은 TX 만 제공된다.
type BandwidthResponse struct {
	GeneratedAt string         `json:"generated_at"`
	Window      string         `json:"window"`
	Pods        []BandwidthPod `json:"pods"`
	// Nodes 는 NIC 포화 판단 근거다. namespace 필터와 무관하게 항상 클러스터 전체 pod 합계로
	// 산출해 필터로 합계가 왜곡되지 않게 한다.
	Nodes   []BandwidthNode `json:"nodes"`
	Summary string          `json:"summary"`
}

// BandwidthPod 는 한 pod 의 방향·관점별 대역폭이다. NicTxBytesPerSec 는 NIC 관점 (skb 단위) TX 로,
// TSO/GSO 나 재전송으로 l4 관점 (syscall 단위) 과 다를 수 있다.
type BandwidthPod struct {
	Namespace        string   `json:"namespace"`
	Pod              string   `json:"pod"`
	Node             string   `json:"node"`
	RxBytesPerSec    float64  `json:"rx_bytes_per_sec"`
	TxBytesPerSec    float64  `json:"tx_bytes_per_sec"`
	NicTxBytesPerSec *float64 `json:"nic_tx_bytes_per_sec,omitempty"`
}

// BandwidthNode 는 한 노드의 pod 합계 대역폭과 NIC capacity 대비 사용률이다. NicUtilization 은
// NIC 관점 TX 합계 / capacity 로, capacity 미수집 노드는 필드가 생략된다.
type BandwidthNode struct {
	Node                   string   `json:"node"`
	RxBytesPerSec          float64  `json:"rx_bytes_per_sec"`
	TxBytesPerSec          float64  `json:"tx_bytes_per_sec"`
	NicTxBytesPerSec       *float64 `json:"nic_tx_bytes_per_sec,omitempty"`
	NicCapacityBytesPerSec *float64 `json:"nic_capacity_bytes_per_sec,omitempty"`
	NicUtilization         *float64 `json:"nic_utilization,omitempty"`
}

// bandwidthPodKey 는 pod 병합 키다.
type bandwidthPodKey struct {
	node      string
	namespace string
	pod       string
}

// GetBandwidth godoc
// @Summary      pod 대역폭 조회
// @Description  pod 단위 RX/TX 대역폭 (bytes/sec) 을 rate(netobs_pod_bytes_total[5m]) 로 반환한다. RX 는 l4 관점만 수집되고 NIC 관점은 TX 만 제공된다. nodes 는 namespace 필터와 무관한 클러스터 전체 합계와 NIC capacity 대비 사용률로 NIC 포화 판단 근거를 제공한다.
// @Tags         network
// @Produce      json
// @Param        namespace  query  string  false  "src_namespace 필터"
// @Param        node       query  string  false  "단일 노드 필터 (DNS-1123 형식, 생략 시 전체)"
// @Param        limit      query  int     false  "상위 N pod (합산 대역폭 내림차순, 기본 50)"
// @Param        at         query  string  false  "평가 시점 (RFC3339 또는 unix seconds, 생략 시 현재)"
// @Success      200  {object}  BandwidthResponse
// @Failure      400  {object}  apicommon.ErrorBody
// @Failure      500  {object}  apicommon.ErrorBody
// @Router       /api/v1/bandwidth [get]
func (h *SynthesisHandler) GetBandwidth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nsFilter := strings.TrimSpace(q.Get("namespace"))
	if _, err := parseNamespaceParam(nsFilter); err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_namespace", err.Error())
		return
	}
	// #447 limit 파싱 통일. 종전에는 파싱 오류를 침묵 흡수해 기본값을 썼는데, swagger 가
	// 범위를 명시하므로 다른 목록 핸들러(flows / drops / incidents)와 동일하게 파싱 불가를
	// 400 으로 돌려준다. 범위 초과 clamp 와 기본값 정책은 ParseLimit 이 동일하게 유지한다.
	limit, lok := apicommon.ParseLimit(r, 50, 500)
	if !lok {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_limit", "limit 은 정수여야 합니다")
		return
	}
	node, err := parseNodeParam(strings.TrimSpace(q.Get("node")))
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_node", err.Error())
		return
	}

	evalCtx, evalAt, ok := applyAtParam(w, r, r.Context())
	if !ok {
		return
	}

	resp := BandwidthResponse{
		GeneratedAt: evalAt.Format(time.RFC3339),
		Window:      "5m",
		Pods:        []BandwidthPod{},
		Nodes:       []BandwidthNode{},
	}

	if h.querier == nil {
		resp.Summary = "대역폭 데이터 없음"
		apicommon.WriteJSON(w, resp)
		return
	}

	ctx, cancel := context.WithTimeout(evalCtx, 5*time.Second)
	defer cancel()

	// namespace 는 기존 규약대로 %q 이스케이프로 PromQL label matcher 에 밀어 Prometheus 측에서
	// 필터한다. #263 node 필터도 같은 selector 에 병합해 pod 쿼리를 노드로 좁힌다.
	// namespace 는 parseNamespaceParam 검증을 통과한 값이라 PromQL %q 결합이 안전하다 (#409 단일 정책).
	nsMatcher := ""
	if nsFilter != "" {
		nsMatcher = fmt.Sprintf("src_namespace=%q", nsFilter)
	}
	selector := promSelector(nsMatcher, nodeMatcher(node))
	podQuery := fmt.Sprintf("sum by(node, src_namespace, src_pod, direction, layer) (rate(netobs_pod_bytes_total%s[5m]))", selector)

	// node 합계와 capacity 는 namespace 와 무관한 노드 단위 집계라 node 필터만 적용한다.
	nodeSel := promSelector(nodeMatcher(node))

	// 주 소스인 pod 대역폭은 직접 조회해 실패를 500 으로 구분한다. node 합계와 capacity 는 포화
	// 판단 보조라 병렬 조회 후 실패 (nil) 를 필드 생략으로 graceful 처리한다.
	podSamples, err := h.querier.Query(ctx, podQuery)
	if err != nil {
		apicommon.WriteError(w, http.StatusInternalServerError, "query_failed", fmt.Sprintf("Prometheus 쿼리 실행 실패: %v", err))
		return
	}
	extras, _ := h.queryParallel(ctx,
		fmt.Sprintf("sum by(node, direction, layer) (rate(netobs_pod_bytes_total%s[5m]))", nodeSel),
		"netobs_node_nic_capacity_bytes_per_sec"+nodeSel,
	)

	pods := map[bandwidthPodKey]*BandwidthPod{}
	order := []bandwidthPodKey{}
	for _, sm := range podSamples {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		k := bandwidthPodKey{node: sm.Labels["node"], namespace: sm.Labels["src_namespace"], pod: sm.Labels["src_pod"]}
		p, ok := pods[k]
		if !ok {
			p = &BandwidthPod{Namespace: k.namespace, Pod: k.pod, Node: k.node}
			pods[k] = p
			order = append(order, k)
		}
		switch {
		case sm.Labels["direction"] == "ingress" && sm.Labels["layer"] == "l4":
			p.RxBytesPerSec = sm.Value
		case sm.Labels["direction"] == "egress" && sm.Labels["layer"] == "l4":
			p.TxBytesPerSec = sm.Value
		case sm.Labels["direction"] == "egress" && sm.Labels["layer"] == "nic":
			v := sm.Value
			p.NicTxBytesPerSec = &v
		}
	}
	for _, k := range order {
		resp.Pods = append(resp.Pods, *pods[k])
	}
	sort.Slice(resp.Pods, func(i, j int) bool {
		ti := resp.Pods[i].RxBytesPerSec + resp.Pods[i].TxBytesPerSec
		tj := resp.Pods[j].RxBytesPerSec + resp.Pods[j].TxBytesPerSec
		if ti != tj {
			return ti > tj
		}
		if resp.Pods[i].Namespace != resp.Pods[j].Namespace {
			return resp.Pods[i].Namespace < resp.Pods[j].Namespace
		}
		return resp.Pods[i].Pod < resp.Pods[j].Pod
	})
	if len(resp.Pods) > limit {
		resp.Pods = resp.Pods[:limit]
	}

	nodes := map[string]*BandwidthNode{}
	nodeOrder := []string{}
	for _, sm := range extras[0] {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) {
			continue
		}
		name := sm.Labels["node"]
		n, ok := nodes[name]
		if !ok {
			n = &BandwidthNode{Node: name}
			nodes[name] = n
			nodeOrder = append(nodeOrder, name)
		}
		switch {
		case sm.Labels["direction"] == "ingress" && sm.Labels["layer"] == "l4":
			n.RxBytesPerSec = sm.Value
		case sm.Labels["direction"] == "egress" && sm.Labels["layer"] == "l4":
			n.TxBytesPerSec = sm.Value
		case sm.Labels["direction"] == "egress" && sm.Labels["layer"] == "nic":
			v := sm.Value
			n.NicTxBytesPerSec = &v
		}
	}
	for _, sm := range extras[1] {
		if math.IsNaN(sm.Value) || math.IsInf(sm.Value, 0) || sm.Value <= 0 {
			continue
		}
		if n, ok := nodes[sm.Labels["node"]]; ok {
			v := sm.Value
			n.NicCapacityBytesPerSec = &v
			if n.NicTxBytesPerSec != nil {
				u := *n.NicTxBytesPerSec / v
				n.NicUtilization = &u
			}
		}
	}
	sort.Strings(nodeOrder)
	for _, name := range nodeOrder {
		resp.Nodes = append(resp.Nodes, *nodes[name])
	}

	resp.Summary = summarizeBandwidth(resp)
	apicommon.WriteJSON(w, resp)
}

// summarizeBandwidth 는 최다 대역폭 pod 와 최고 NIC 사용률 노드 기준 한 줄 요약을 만든다.
func summarizeBandwidth(r BandwidthResponse) string {
	if len(r.Pods) == 0 {
		return "대역폭 데이터 없음"
	}
	top := r.Pods[0]
	s := fmt.Sprintf("pod %d개, 최다 %s/%s (RX %.2f + TX %.2f Mbps)",
		len(r.Pods), top.Namespace, top.Pod, top.RxBytesPerSec*8/1e6, top.TxBytesPerSec*8/1e6)
	var worst *BandwidthNode
	for i := range r.Nodes {
		if r.Nodes[i].NicUtilization == nil {
			continue
		}
		if worst == nil || *r.Nodes[i].NicUtilization > *worst.NicUtilization {
			worst = &r.Nodes[i]
		}
	}
	if worst != nil {
		s += fmt.Sprintf(", 최고 NIC 사용률 %s %.1f%%", worst.Node, *worst.NicUtilization*100)
	}
	return s
}
