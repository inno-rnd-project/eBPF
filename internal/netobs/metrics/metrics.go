package metrics

import (
	"strconv"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/kube"
	"netobs/internal/netobs/metadata"
	"netobs/internal/netobs/types"
)

// podMetricsEnabled / dstClassifier 는 startup 시점에 main 이 한 번 설정하고 이후 모든 Record 호출이
// 읽기만 하는 토글이다. plain global 도 Go memory model 의 goroutine spawn happens-before 로 안전
// 하지만 podbytes.Collector.bpfMap 의 atomic.Pointer 사용 패턴과 일관성을 맞추고 race detector 가
// false-positive 없이 검증 가능하도록 atomic 타입으로 둔다.
var (
	podMetricsEnabled atomic.Bool
	dstClassifier     atomic.Pointer[metadata.DstLabelClassifier]
)

// 기본값은 podMetricsEnabled=true. atomic.Bool 의 zero value 가 false 이므로 init 에서 명시적으로
// true 로 올려 main 이 SetPodMetricsEnabled 를 호출하기 전 윈도우에서도 호환되는 동작을 한다.
func init() {
	podMetricsEnabled.Store(true)
}

// SetPodMetricsEnabled은 pod-instance 레벨 메트릭 기록 여부를 전환하며, 반드시 Record가 호출되기
// 전 (main startup 단계) 에 호출되어야 한다.
// dropFlowGuard 는 netobs_drop_events_flow_total 의 emit cardinality 가드다. SetDropFlowGuard 가
// startup 시점에 설정하지 않으면 nil 로 두어 emit 자체가 skip 된다 (#64 의 safe default).
var dropFlowGuard *DropFlowGuard

// SetDropFlowGuard 는 main agent 가 config 의 DropFlowAllowNamespaces 와 DropFlowMaxActive 로 가드를
// 구성해 본 함수로 wire-up 한다. nil 을 전달하면 metric emit 이 명시적으로 disable 된다.
func SetDropFlowGuard(g *DropFlowGuard) {
	dropFlowGuard = g
}

// tcpStateAggregator 는 #65 의 receive path TCP 상태 sample 을 Pod 단위 gauge 로 집계한다. main
// agent 가 SetTCPStateAggregator 로 startup 시 주입하며, 미설정 시 nil 로 두어 rcv stage event 가
// 들어와도 emit 자체가 skip 된다.
var tcpStateAggregator *TCPStateAggregator

// SetTCPStateAggregator 는 main agent 가 NewTCPStateAggregator 결과를 prometheus.Registerer 에
// 등록한 뒤 본 함수로 wire-up 해 Record 가 rcv_* stage event 의 TCP 상태를 dispatch 하게 한다.
func SetTCPStateAggregator(a *TCPStateAggregator) {
	tcpStateAggregator = a
}

// dropClockOffsetNs 는 #142 의 drop 발생 시점 gauge 가 BPF monotonic ts_ns 를 wall-clock unix ns 로
// 변환할 때 더하는 offset 이다. cmd/netobs-agent 가 startup 시 (time.Now - CLOCK_MONOTONIC) 차이를
// SetDropClockOffset 으로 주입한다. bpf_ktime_get_ns 가 CLOCK_MONOTONIC 기준이라 본 offset 을 더하면
// boot 기준 monotonic 값이 unix epoch 기준 wall-clock 으로 환산된다. 미설정 (0) 이면 ts_ns 가 그대로
// 초 변환되어 monotonic 값이 노출되나 정상 wire-up 에서는 항상 주입된다.
var dropClockOffsetNs atomic.Int64

// SetDropClockOffset 은 #142 의 monotonic→wall 변환 offset (nanoseconds) 을 설정한다. main agent 가
// startup 시 1회 호출한다.
func SetDropClockOffset(ns int64) {
	dropClockOffsetNs.Store(ns)
}

// dropWallSeconds 는 BPF monotonic ts_ns 를 wall-clock unix seconds 로 변환한다. offset 미설정 시
// monotonic 값을 그대로 초 변환한다.
func dropWallSeconds(tsNs uint64) float64 {
	return float64(int64(tsNs)+dropClockOffsetNs.Load()) / 1e9
}

func SetPodMetricsEnabled(v bool) {
	podMetricsEnabled.Store(v)
}

// SetDstClassifier는 dst 라벨 산출 정책을 주입한다. POD_FLOW_DST_ENABLED / POD_FLOW_DST_UID_ALLOW_
// NAMESPACES 두 토글을 main 이 classifier 로 wrap 해 본 함수로 한 번 전달하면 이후 모든 Record 호출
// 이 동일 정책으로 dst 라벨을 채운다. classifier 가 nil 이면 Labels 가 ("","","") 를 반환하므로
// dst 라벨은 도입 전과 호환되는 빈 값으로 emit 된다.
func SetDstClassifier(c *metadata.DstLabelClassifier) {
	dstClassifier.Store(c)
}

// dstLabels는 dstClassifier nil-safe wrapper다. atomic.Pointer.Load 는 Store 전이거나 명시적 nil
// store 후에 nil 을 반환할 수 있어 별도 분기 없이 라벨 슬라이스에 그대로 append 가능하도록 빈 값을
// 돌려준다. outcome 은 self-observe counter 의 bucket 라벨로 사용된다.
func dstLabels(p kube.PodIdentity) (ns, workload, podUID, outcome string) {
	c := dstClassifier.Load()
	if c == nil {
		return "", "", "", outcomeDisabled
	}
	return c.Labels(p)
}

var (
	legacyEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_events_total",
			Help: "Total custom eBPF events by stage",
		},
		[]string{"stage"},
	)

	legacyLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_stage_latency_seconds",
			Help:    "Latency of selected kernel stages",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"stage"},
	)

	legacyDropTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_drop_total",
			Help: "Drop events by kernel reason code",
		},
		[]string{"reason"},
	)

	// dst_* 라벨은 모든 labeled 메트릭의 라벨 슬라이스 끝에 추가된다. 기존 PromQL은 sum by/without으로
	// 라벨 변화를 자연 수용하며, dst 라벨 master switch 가 꺼진 운영 모드에서는 세 라벨이 빈 문자열로
	// emit 되어 cardinality 가 도입 전 수준 (단일 빈 라벨 셋) 으로 collapse 된다.

	stageEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_stage_events_labeled_total",
			Help: "Enriched eBPF events by stage, node, workload, traffic scope, and dst peer. src/dst label semantics: src is the local Pod owning the cgroup, dst is the traffic peer in flow direction. All three dst_* labels are empty when PodFlowDstEnabled=false (master switch off). When the dst_pod_uid allow-list does not include a Pod destination's namespace, only dst_pod_uid is empty while dst_namespace and dst_workload remain populated (cardinality control). Failed resolution is marked dst_workload=\"_unresolved\". To avoid double-counting same-node Pod-to-Pod flows aggregate by direction=egress only.",
		},
		[]string{"stage", "node", "src_namespace", "src_workload", "traffic_scope", "direction", "dst_namespace", "dst_workload"},
	)

	stageLatencyLabeled = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_stage_latency_labeled_seconds",
			Help:    "Enriched latency by stage, node, workload, traffic scope, and dst peer. See netobs_stage_events_labeled_total help for src/dst semantics.",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"stage", "node", "src_namespace", "src_workload", "traffic_scope", "direction", "dst_namespace", "dst_workload"},
	)

	dropEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_drop_events_labeled_total",
			Help: "Enriched drop events with human-readable drop reason, category, and dst peer.",
		},
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction", "drop_reason", "drop_category", "dst_namespace", "dst_workload"},
	)

	retransEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_retrans_events_labeled_total",
			Help: "Enriched retrans events by node, workload, traffic scope, and dst peer.",
		},
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction", "dst_namespace", "dst_workload"},
	)

	// dropEventsFlow 는 #64 의 drop flow 5-tuple context 메트릭이다. 기존 dropEventsLabeled 가
	// workload 단위로 emit 하는 반면 본 메트릭은 5-tuple (src_ip, src_port, dst_ip, dst_port,
	// protocol) 라벨로 정확한 connection 식별을 제공한다. high cardinality 메트릭이라 emit 은
	// namespace allow-list 와 top-N flow sampling 가드를 거친다 (다음 commit 에서 추가).
	dropEventsFlow = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_drop_events_flow_total",
			Help: "Drop events with 5-tuple flow context for #64. Emitted only for namespaces in NETOBS_DROP_FLOW_ALLOW_NAMESPACES and limited to top-N active flows for cardinality control. Use this metric to identify which specific connection's packets were dropped, complementing the workload-level netobs_drop_events_labeled_total. #103: ip_version 라벨로 IPv4/IPv6 분리.",
		},
		[]string{"node", "src_namespace", "src_workload", "src_pod", "traffic_scope", "direction", "drop_reason", "drop_category", "protocol", "src_ip", "src_port", "dst_ip", "dst_port", "ip_version"},
	)

	// dropLastTimestamp 는 #142 의 drop 발생 시점 (wall-clock unix seconds) gauge 다. counter rate 가
	// 잃는 "원인이 언제 발생했는가" 정보를 보존해 time() - netobs_drop_last_timestamp_seconds 로 마지막
	// drop 이후 경과를 산정 가능하게 한다. BPF 가 캡처한 monotonic ts_ns 를 dropWallSeconds 가 boot
	// offset 으로 wall-clock 으로 변환한 값이다. dropEventsFlow 와 동일한 5-tuple flow 라벨 셋과
	// dropFlowGuard (allow-list + top-N LRU) 를 공유해 cardinality 가 동일하게 통제된다.
	dropLastTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "netobs_drop_last_timestamp_seconds",
			Help: "Wall-clock unix timestamp (seconds) of the most recent packet drop per 5-tuple flow (#142). Converted from the BPF monotonic ts_ns via a boot-time offset. Preserves the drop occurrence time that counter rate windows lose; use time() - netobs_drop_last_timestamp_seconds to measure staleness since the last drop. Shares the netobs_drop_events_flow_total label set and dropFlowGuard (NETOBS_DROP_FLOW_ALLOW_NAMESPACES allow-list + top-N LRU) so cardinality is controlled identically.",
		},
		[]string{"node", "src_namespace", "src_workload", "src_pod", "traffic_scope", "direction", "drop_reason", "drop_category", "protocol", "src_ip", "src_port", "dst_ip", "dst_port", "ip_version"},
	)

	// pod-level 메트릭에는 dst_pod_uid 까지 노출된다. POD_FLOW_DST_UID_ALLOW_NAMESPACES 토글에 등록된
	// namespace 의 dst Pod 흐름에 한해 값이 채워지고 그 외에는 빈 문자열로 emit 되어 cardinality 가
	// 통제된다.

	podStageEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_pod_stage_events_labeled_total",
			Help: "Enriched eBPF events by stage, source pod instance, and dst peer (including dst_pod_uid when target namespace is in allow-list).",
		},
		[]string{"stage", "node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction", "dst_namespace", "dst_workload", "dst_pod_uid"},
	)

	podStageLatencyLabeled = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_pod_stage_latency_labeled_seconds",
			Help:    "Enriched latency by stage, source pod instance, and dst peer.",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"stage", "node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction", "dst_namespace", "dst_workload", "dst_pod_uid"},
	)

	// #121 TSO/GSO 환경 send path 의 segment 누적 latency histogram. tcp_transmit_skb 의 모든 segment
	// 호출 latency 의 합산이며 sendmsg 사이클 1회 당 1 sample emit 된다. raw stage_latency 의 첫 segment
	// 만 측정하는 한계를 보완해 운영자가 large message 의 transmit_skb 처리 비용 합산을 정확히 추적한다.
	// 라벨 셋은 podStageLatencyLabeled 에서 stage 라벨 제외 한 9종 으로 cardinality 정합 유지.
	sendPathFullLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "netobs_send_path_full_latency_seconds",
			Help:    "TSO/GSO 환경 send path 의 segment 누적 latency 합산 (seconds). tcp_transmit_skb 의 모든 segment 호출 latency 합산이며 sendmsg 사이클 1회 당 1 sample emit 된다. raw netobs_pod_stage_latency_labeled_seconds 의 첫 segment 만 측정 하는 한계 를 보완 한다.",
			Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20),
		},
		[]string{"node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction", "dst_namespace", "dst_workload", "dst_pod_uid"},
	)

	// #121 TSO/GSO 환경 send path 의 누적 segment 개수 counter. tcp_transmit_skb 호출 횟수 의 합산
	// 이며 segment_count > 1 누적 증가 는 TSO/GSO 활성 환경 의 large message 분할 신호다. cardinality
	// 폭증 회피 위해 segment_count 자체를 라벨로 두지 않고 Add(float64(segment_count)) 로 누적 합산
	// 한다.
	sendPathSegmentCountTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_send_path_segment_count_total",
			Help: "TSO/GSO 환경 send path 의 누적 segment 개수. tcp_transmit_skb 호출 횟수 의 합산이며 segment_count > 1 누적 증가 는 large message 의 multi-segment 분할 신호 다.",
		},
		[]string{"node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction", "dst_namespace", "dst_workload", "dst_pod_uid"},
	)

	// dstClassifierEmits 는 dst 라벨 분류 outcome 분포를 카운팅하는 self-observe 메트릭이다.
	// allow-list 가 잘못 설정돼 단명 Pod 의 churn 으로 dst_pod_uid 가 폭증하는 경우 rate(pod_with_uid)
	// 가 비정상적으로 높게 잡혀 운영자가 cardinality bomb 징후를 조기에 발견할 수 있다. disabled 버킷
	// 은 startup 직후 classifier 미설정 윈도우 또는 PodFlowDstEnabled=false 운영 모드에서 증가한다.
	dstClassifierEmits = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_dst_classifier_emits_total",
			Help: "Count of dst label classifications by outcome bucket. Buckets: disabled (classifier nil or master switch off), external, unresolved, service, pod_with_uid (allow-list namespace), pod_without_uid (non allow-list namespace), other (Node identity or unknown). Use rate(pod_with_uid) to detect cardinality bombs from misconfigured allow-list namespaces with high pod churn.",
		},
		[]string{"outcome"},
	)

	// nicCapacityBytesPerSec 는 노드 NIC 이론 capacity (bytes/sec) 를 노출하는 static gauge다.
	// correlation 진단의 pod:network_throughput_score:5m recording rule이 본 메트릭을 분모로 써서
	// pod 단위 traffic을 0-1 score로 정규화한다. 운영자가 노드별 실제 NIC 사양과 다르면 NIC_CAPACITY_
	// BYTES_PER_SEC env / -nic-capacity-bytes CLI flag 로 override 한다. startup 1회 Set으로 결정.
	nicCapacityBytesPerSec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "netobs_node_nic_capacity_bytes_per_sec",
			Help: "Node NIC theoretical capacity in bytes/sec, sourced from NIC_CAPACITY_BYTES_PER_SEC env (default 1.25e9 = 10 GbE). Consumed by correlation recording rules (pod:network_throughput_score:5m) as the saturation denominator. Override per node via env to match actual hardware.",
		},
		[]string{"node"},
	)
)

// outcome 라벨 값. classifier 가 반환하는 7 가지 bucket 을 string constant 로 두어 metrics / metadata
// 양쪽에서 동일 표기를 보장한다.
const (
	outcomeDisabled      = "disabled"
	outcomeExternal      = "external"
	outcomeUnresolved    = "unresolved"
	outcomeService       = "service"
	outcomePodWithUID    = "pod_with_uid"
	outcomePodWithoutUID = "pod_without_uid"
	outcomeOther         = "other"
)

func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		legacyEventsTotal,
		legacyLatencySeconds,
		legacyDropTotal,
		stageEventsLabeled,
		stageLatencyLabeled,
		dropEventsLabeled,
		dropEventsFlow,
		dropLastTimestamp,
		retransEventsLabeled,
		podStageEventsLabeled,
		podStageLatencyLabeled,
		sendPathFullLatencySeconds,
		sendPathSegmentCountTotal,
		dstClassifierEmits,
		nicCapacityBytesPerSec,
		bpfProgramLoaded,
		bpfProgramAttachTotal,
		bpfProgramAttachRetryTotal,
		bpfRingbufDropsTotal,
		bpfMapUtilizationRatio,
		informerSyncLagSeconds,
		dropStackTotal,
		dropStackResolverCacheHits,
		dropStackResolverCacheMisses,
	)
}

// SetNICCapacityBytesPerSec는 노드 NIC 이론 capacity gauge를 startup 시점에 1회 설정한다. 본 메트릭은
// scrape 시점마다 정적 값을 반환하므로 호출 횟수와 무관하게 cardinality 가 node 단위로 통제된다.
func SetNICCapacityBytesPerSec(node string, capacity float64) {
	nicCapacityBytesPerSec.WithLabelValues(node).Set(capacity)
}

func label(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

func podNameLabel(p kube.PodIdentity) string {
	if p.PodName != "" {
		return p.PodName
	}
	return "unknown"
}

func podUIDLabel(p kube.PodIdentity) string {
	if p.PodUID != "" {
		return p.PodUID
	}
	return "unknown"
}

func Record(ev types.EnrichedEvent) {
	stage := label(ev.Stage)

	legacyEventsTotal.WithLabelValues(stage).Inc()

	// dst 라벨은 classifier 가 빈 문자열을 반환할 수 있고 (master switch off, UID 게이트 외 케이스)
	// 그 의도는 cardinality collapse 이므로 label() 의 "unknown" 대치를 우회한다. outcome 은
	// self-observe counter 에 기록되어 운영자가 cardinality bomb 징후를 추적 가능하다.
	dstNs, dstWl, dstUID, dstOutcome := dstLabels(ev.Dst)
	dstClassifierEmits.WithLabelValues(dstOutcome).Inc()

	common := []string{
		stage,
		label(ev.ObservedNodeLabel()),
		label(ev.SourceNamespaceLabel()),
		label(ev.SourceWorkloadLabel()),
		label(ev.TrafficScope),
		label(ev.Direction),
		dstNs,
		dstWl,
	}

	stageEventsLabeled.WithLabelValues(common...).Inc()

	if podMetricsEnabled.Load() && ev.Src.IsPod() {
		podCommon := []string{
			stage,
			label(ev.ObservedNodeLabel()),
			label(ev.SourceNamespaceLabel()),
			label(podNameLabel(ev.Src)),
			label(podUIDLabel(ev.Src)),
			label(ev.TrafficScope),
			label(ev.Direction),
			dstNs,
			dstWl,
			dstUID,
		}
		podStageEventsLabeled.WithLabelValues(podCommon...).Inc()

		switch ev.Raw.Stage {
		case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ,
			types.StageTcpWriteXmit, types.StageTcpTransmitSkb:
			latencySec := float64(ev.Raw.LatencyUs) / 1_000_000.0
			podStageLatencyLabeled.WithLabelValues(podCommon...).Observe(latencySec)
		}

		// #121 sendmsg_ret stage 의 segment 누적 latency 와 segment_count 를 별도 메트릭으로 emit.
		// segment_count > 0 가드 로 seg_accum entry 가 부재 한 sendmsg (tcp_transmit_skb 미호출) 의
		// 0 sample 노이즈 를 차단 한다. raw stage_latency 의 첫 segment 측정 흐름 과 독립. podCommon
		// 의 첫 항목 (stage 라벨) 을 제외 한 9 종 라벨 셋 으로 cardinality 정합 유지.
		if ev.Raw.Stage == types.StageSendmsgRet && ev.Raw.SegmentCount > 0 {
			fullLatencySec := float64(ev.Raw.FullLatencyNs) / 1_000_000_000.0
			sendPathFullLatencySeconds.WithLabelValues(podCommon[1:]...).Observe(fullLatencySec)
			sendPathSegmentCountTotal.WithLabelValues(podCommon[1:]...).Add(float64(ev.Raw.SegmentCount))
		}
	}

	switch ev.Raw.Stage {
	case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ,
		types.StageTcpWriteXmit, types.StageTcpTransmitSkb:
		latencySec := float64(ev.Raw.LatencyUs) / 1_000_000.0
		legacyLatencySeconds.WithLabelValues(stage).Observe(latencySec)
		stageLatencyLabeled.WithLabelValues(common...).Observe(latencySec)

	case types.StageDrop:
		legacyDropTotal.WithLabelValues(strconv.FormatUint(uint64(ev.Raw.Reason), 10)).Inc()
		dropEventsLabeled.WithLabelValues(
			label(ev.ObservedNodeLabel()),
			label(ev.SourceNamespaceLabel()),
			label(ev.SourceWorkloadLabel()),
			label(ev.TrafficScope),
			label(ev.Direction),
			label(ev.DropReasonName),
			label(ev.DropCategory),
			dstNs,
			dstWl,
		).Inc()
		// #64 의 5-tuple drop flow 메트릭은 namespace allow-list 와 top-N LRU 가드를 통과한 경우만
		// emit 한다. guard 가 nil 이면 emit 자체가 skip 되어 cardinality 가 도입 전 수준 (0 series)
		// 으로 유지된다.
		if dropFlowGuard != nil && dropFlowGuard.Admit(
			ev.SourceNamespaceLabel(),
			ev.SrcIPText, ev.Raw.Sport,
			ev.DstIPText, ev.Raw.Dport,
			ev.ProtocolText,
		) {
			// #142 dropEventsFlow (counter) 와 dropLastTimestamp (gauge) 가 동일한 5-tuple flow 라벨
			// 셋을 공유하므로 라벨 슬라이스를 한 번 구성해 양쪽에 전달한다. counter 는 drop 누적을,
			// gauge 는 발생 시점 (wall-clock seconds) 을 노출한다.
			flowLabels := []string{
				label(ev.ObservedNodeLabel()),
				label(ev.SourceNamespaceLabel()),
				label(ev.SourceWorkloadLabel()),
				label(ev.Src.PodName),
				label(ev.TrafficScope),
				label(ev.Direction),
				label(ev.DropReasonName),
				label(ev.DropCategory),
				label(ev.ProtocolText),
				label(ev.SrcIPText),
				strconv.FormatUint(uint64(ev.Raw.Sport), 10),
				label(ev.DstIPText),
				strconv.FormatUint(uint64(ev.Raw.Dport), 10),
				types.IPVersion(ev.Raw.Family),
			}
			dropEventsFlow.WithLabelValues(flowLabels...).Inc()
			// #142 offset 이 미설정 (0) 이면 CLOCK_MONOTONIC 읽기 실패로 wall-clock 변환이 불가한
			// 상태다. 이때 gauge 를 Set 하면 monotonic 값이 "wall-clock unix timestamp" 로 오노출되어
			// time() - netobs_drop_last_timestamp_seconds 가 왜곡되므로 Set 을 skip 해 시리즈를 emit
			// 하지 않는다. 정상 wire-up 에서는 startup 에 0 이 아닌 offset 이 주입되어 항상 Set 된다.
			if dropClockOffsetNs.Load() != 0 {
				dropLastTimestamp.WithLabelValues(flowLabels...).Set(dropWallSeconds(ev.Raw.TsNs))
			}
		}
		// #83 의 stack 메트릭은 별도 namespace allow-list / LRU 가드 (DropStackGuard) 와 resolver
		// 의 ok 결과 양쪽이 통과해야 emit 된다. guard 와 resolver 가 nil 이면 fail-open 으로 emit
		// 자체가 skip 되어 기존 drop 메트릭은 정상 동작을 유지한다.
		if dropStackGuard != nil && dropStackGuard.Admit(
			ev.SourceNamespaceLabel(),
			ev.SrcIPText, ev.Raw.Sport,
			ev.DstIPText, ev.Raw.Dport,
			ev.ProtocolText,
		) {
			recordDropStack(
				label(ev.ObservedNodeLabel()),
				label(ev.SourceNamespaceLabel()),
				label(ev.SourceWorkloadLabel()),
				label(ev.DropReasonName),
				label(ev.DropCategory),
				ev.Raw.StackID,
			)
		}

	case types.StageRetrans:
		retransEventsLabeled.WithLabelValues(
			label(ev.ObservedNodeLabel()),
			label(ev.SourceNamespaceLabel()),
			label(ev.SourceWorkloadLabel()),
			label(ev.TrafficScope),
			label(ev.Direction),
			dstNs,
			dstWl,
		).Inc()

	case types.StageRcvNic, types.StageRcvDemux, types.StageRcvEstablished, types.StageRcvApp:
		// #141 receive path 의 stage 별 커널 처리시간을 송신 경로와 동일한 stage latency histogram 에
		// Observe 한다. RCV_DEMUX 와 RCV_ESTABLISHED 는 L3 진입 기준 누적 커널 처리시간이고 RCV_APP 은
		// established 기준 app pickup 대기라 의미가 다르지만 stage 라벨로 구분되며, dashboard 의
		// receive path 패널이 본 rcv_* stage 시리즈를 쿼리한다. ts_l3 부재 (신규 SYN / listen socket)
		// 케이스는 BPF 가 latency_us=0 으로 emit 하므로 0 sample 이 자연 포함된다. #173 의 rcv_nic 은
		// NIC ingress→L3 구간으로, BPF 가 nic_ingress 와 skb 상관 성공 시에만 emit 하므로 0 sample 이
		// 섞이지 않는다.
		latencySec := float64(ev.Raw.LatencyUs) / 1_000_000.0
		legacyLatencySeconds.WithLabelValues(stage).Observe(latencySec)
		stageLatencyLabeled.WithLabelValues(common...).Observe(latencySec)

		// #65 receive path 의 TCP 상태 sample 을 수신 Pod (ingress event 의 Dst) 단위로 누적한다.
		// aggregator 미설정 (nil) 또는 Dst 가 Pod 가 아닌 케이스 (peer 가 외부 / 노드) 는 emit 자체를
		// skip 해 cardinality 가 클러스터 내 Pod 셋으로만 한정되게 한다. #173 의 rcv_nic 은 동일 sk 의
		// 중복 sample 이라 tcp_state 집계 source 에서 제외하고 기존 3 stage (#65) 에서만 수집한다.
		if ev.Raw.Stage != types.StageRcvNic && tcpStateAggregator != nil && ev.Dst.IsPod() {
			tcpStateAggregator.Observe(TCPStateLabels{
				Namespace: ev.Dst.NamespaceLabel(),
				Pod:       ev.Dst.PodName,
				Node:      ev.ObservedNodeLabel(),
			}, ev.Raw.SndCwnd, ev.Raw.SrttUs, ev.Raw.SndSsthresh)
		}
	}
}
