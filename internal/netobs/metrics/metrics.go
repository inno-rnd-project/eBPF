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
			Help: "Enriched eBPF events by stage, node, workload, traffic scope, and dst peer. src/dst label semantics: src is the local Pod owning the cgroup, dst is the traffic peer in flow direction. Empty dst_* label values are intentional and indicate that PodFlowDstEnabled=false or the dst_pod_uid allow-list does not include the namespace (cardinality control), not failed resolution; failed resolution is marked dst_workload=\"_unresolved\". To avoid double-counting same-node Pod-to-Pod flows aggregate by direction=egress only.",
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
		retransEventsLabeled,
		podStageEventsLabeled,
		podStageLatencyLabeled,
		dstClassifierEmits,
	)
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
		case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ:
			latencySec := float64(ev.Raw.LatencyUs) / 1_000_000.0
			podStageLatencyLabeled.WithLabelValues(podCommon...).Observe(latencySec)
		}
	}

	switch ev.Raw.Stage {
	case types.StageSendmsgRet, types.StageToVeth, types.StageToDevQ:
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
	}
}
