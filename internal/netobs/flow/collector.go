// Package flow 는 #85 의 BPF flow_bytes 누적 맵을 Prometheus counter 로 노출하는 custom collector
// 를 구현 한다. podbytes.Collector 와 동일 패턴 (atomic.Pointer 기반 SetMap, BPF map iterate, throttled
// error log) 을 따르나 cardinality 가드 (FlowGuard) 와 5-tuple 라벨 셋 이 다르다. FlowGuard 의 allow-
// list 가 비어 있으면 collector 가 iterate 자체 를 skip 해 비활성 운영 모드 의 scrape 비용 이 0 으로
// 유지 된다.
package flow

import (
	"log"
	"sync/atomic"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/kube"
	ebpfx "netobs/internal/netobs/ebpf"
	"netobs/internal/netobs/metadata"
	"netobs/internal/netobs/metrics"
	"netobs/internal/netobs/types"
)

// errLogMinInterval 은 silent-skip 에러 로그 의 최소 간격 이다. podbytes.Collector 와 동일 1 분 throttle.
const errLogMinInterval = time.Minute

// CgroupResolver 는 cgroup_id 를 PodIdentity 로 풀어 주는 lookup 인터페이스 다. metadata.Enricher 가
// 본 인터페이스 를 만족 한다.
type CgroupResolver interface {
	ResolveCgroup(cgroupID uint64) (kube.PodIdentity, bool)
}

// IPResolver 는 IP 문자열 을 PodIdentity 로 풀어 주는 lookup 인터페이스 다. kube.Resolver 가 본 인터페이스
// 를 만족 한다. dst 라벨 셋 의 dst_namespace / dst_pod_uid 결정 에 사용 된다.
type IPResolver interface {
	ResolveIP(ip string) kube.PodIdentity
}

// dirLabel 매핑. flow_bytes 의 BPF enum (NETOBS_DIR_EGRESS=0, NETOBS_DIR_INGRESS=1) 을 사람 읽을 수
// 있는 라벨 문자열 로 변환 한다.
const (
	dirEgress  = "egress"
	dirIngress = "ingress"
	dirUnknown = "unknown"
)

func directionLabel(v uint8) string {
	switch v {
	case 0:
		return dirEgress
	case 1:
		return dirIngress
	default:
		return dirUnknown
	}
}

// Collector 는 BPF flow_bytes 맵을 scrape 시점 에 iterate 해 netobs_flow_bytes_total 을 emit 한다.
// FlowGuard 의 allow-list 가 비어 있으면 iterate 자체 를 skip 한다. dstClassifier 는 dst 측 라벨 (dst_
// namespace, dst_pod_uid) 의 master switch 와 allow-list 정책 을 그대로 차용 한다.
type Collector struct {
	bpfMap        atomic.Pointer[cebpf.Map]
	cgroup        CgroupResolver
	ip            IPResolver
	guard         *metrics.FlowGuard
	dstClassifier *metadata.DstLabelClassifier
	node          string
	enabled       bool

	bytesDesc *prometheus.Desc

	lastIterErrLogNs atomic.Int64
}

// New 는 Collector 를 구성 한다. guard 가 nil 또는 allow-list 가 비어 있으면 본 collector 는 어떤
// 시리즈 도 emit 하지 않는다. dstClassifier 가 nil 이면 dst 라벨 셋 이 모두 빈 문자열 로 emit 된다.
// cgroup 은 cgroup_id → PodIdentity 매핑 을 (typically metadata.Enricher), ip 는 IP → PodIdentity
// 매핑 을 (typically kube.Resolver) 담당 한다. ip 가 nil 이면 dst 라벨 두 칸 이 빈 문자열 로 채워진다.
func New(cgroup CgroupResolver, ip IPResolver, guard *metrics.FlowGuard, dstClassifier *metadata.DstLabelClassifier, node string, enabled bool) *Collector {
	labels := []string{
		"node",
		"src_namespace", "src_workload", "src_pod", "src_pod_uid",
		"src_ip", "src_port",
		"dst_namespace", "dst_pod_uid",
		"dst_ip", "dst_port",
		"protocol",
		"direction",
		// #103 IPv6 와 UDP 확장. ip_version 라벨 값 은 "4" 또는 "6" (NETOBS_AF_INET / AF_INET6). 기존
		// PromQL 의 sum by 절 은 본 라벨 을 자연 흡수 해 IPv4 / IPv6 합산 으로 동작.
		"ip_version",
	}
	return &Collector{
		cgroup:        cgroup,
		ip:            ip,
		guard:         guard,
		dstClassifier: dstClassifier,
		node:          node,
		enabled:       enabled,
		bytesDesc: prometheus.NewDesc(
			"netobs_flow_bytes_total",
			"#85 Pod 간 정상 flow 의 5-tuple RX/TX bytes counter. FlowGuard allow-list 통과 시에만 emit되며 namespace 와 LRU 1024 sampling 으로 cardinality 가 제한된다.",
			labels, nil,
		),
	}
}

// SetMap 은 BPF 로드 이후 ebpfx.Runtime 이 노출한 FlowBytes 맵을 주입 한다. 본 호출 전 까지 Collect
// 는 no-op 이다.
func (c *Collector) SetMap(m *cebpf.Map) {
	c.bpfMap.Store(m)
}

// Describe 는 prometheus.Collector 인터페이스 를 만족 한다.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bytesDesc
}

// Collect 는 BPF flow_bytes 맵을 iterate 해 FlowGuard 통과 entry를 netobs_flow_bytes_total로 emit
// 한다. guard 의 allow-list가 비어 있어 어떤 namespace도 admit되지 않는 경우 iterate 자체를 skip
// 해 BPF map iteration 비용을 0으로 유지한다. iterate 도중 에러가 발생하면 부분 결과를 ch로 보내지
// 않고 통째로 폐기해 Prometheus counter reset 오해를 회피한다.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if !c.enabled || c.guard == nil {
		return
	}
	m := c.bpfMap.Load()
	if m == nil {
		return
	}

	var key ebpfx.NetObsNetobsFlowKey
	var value ebpfx.NetObsNetobsFlowValue

	// 1단계: BPF map iterate 결과를 emitted label set 기준으로 aggregate 한다. cgroup_id 는 BPF key
	// 에 포함되지만 metric label 셋에는 없 으므로 동일 pod 의 multi-container 등으로 cgroup_id 가
	// 다르나 동일 5-tuple / direction 을 갖는 entries 가 발생 하면 동일 label set 의 중복 metric 으로
	// Prometheus 에서 거부 된다. local pod (cgroup → PodIdentity) 단위로 bytes 를 합산 해 단일 시리즈
	// 로 normalize 한다.
	agg := make(map[aggKey]*aggValue)
	iter := m.Iterate()
	for iter.Next(&key, &value) {
		c.mergeEntry(agg, key, value)
	}
	if err := iter.Err(); err != nil {
		throttledLog(&c.lastIterErrLogNs, "flow: bpf map iterate failed, discarding partial scrape: %v", err)
		return
	}

	// 2단계: aggregate 결과를 ch 채널로 emit. iter.Err 가 nil 인 경우에만 도달 하므로 부분 결과 가
	// 노출 되지 않는다.
	for k, v := range agg {
		ch <- prometheus.MustNewConstMetric(
			c.bytesDesc,
			prometheus.CounterValue,
			float64(v.bytes),
			c.node,
			v.srcNS, v.srcWorkload, v.srcPod, v.srcUID,
			k.srcIP, formatPort(k.srcPort),
			v.dstNS, v.dstUID,
			k.dstIP, formatPort(k.dstPort),
			k.protocol,
			k.direction,
			k.ipVersion,
		)
	}
}

// aggKey 는 emitted label 셋 중 fixed 부분 (5-tuple + direction + protocol + ip_version) 의 합성 키 다.
// 동일 키 의 여러 BPF entry 는 bytes 합산 후 단일 시리즈로 emit 된다. #103 IPv6 확장 으로 ipVersion
// 필드 추가.
type aggKey struct {
	srcIP     string
	srcPort   uint16
	dstIP     string
	dstPort   uint16
	protocol  string
	direction string
	ipVersion string
	// dedupeUID 는 동일 키 의 multi-cgroup 케이스 에서 local pod UID 까지 같은 entry 만 합치도록 가드
	// 한다. 다른 PodUID 면 별개 series (라벨 충돌은 없 으나 dedupe 의도 외) 가 된다.
	dedupeUID string
}

// aggValue 는 합산 bytes 와 swap 결정 된 src / dst 측 라벨 셋 을 함께 보관 한다.
type aggValue struct {
	bytes       uint64
	srcNS       string
	srcWorkload string
	srcPod      string
	srcUID      string
	dstNS       string
	dstUID      string
}

// mergeEntry 는 단일 BPF map entry 를 라벨 변환 / FlowGuard 검증 후 agg 맵에 합산 한다. 가드 실패 또는
// 미해상 entry 는 skip 된다.
//
// ingress 의 경우 BPF 가 sk 의 skc_rcv_saddr / skc_daddr 를 그대로 채워 src = local (수신측) / dst =
// remote (송신측) 형태로 저장 한다. 본 함수 는 프로젝트 내 다른 메트릭 (emit_rcv_event 의 saddr/daddr
// swap 패턴, drop / stage 메트릭) 과 라벨 의미 정합 을 위해 ingress 시 src 와 dst 를 swap 해 src 가
// 항상 sender, dst 가 항상 receiver 가 되도록 한다. FlowGuard.Admit 의 namespace 가드 는 swap 과
// 무관 하게 local pod 의 namespace 로 검사 해 운영자 가 "본 노드 의 allow-list namespace pod 의 모든
// flow (양 방향)" 를 단일 allow-list 로 추적 가능 하게 한다.
func (c *Collector) mergeEntry(agg map[aggKey]*aggValue, key ebpfx.NetObsNetobsFlowKey, value ebpfx.NetObsNetobsFlowValue) {
	localPod, ok := c.cgroup.ResolveCgroup(key.CgroupId)
	if !ok || !localPod.IsPod() || localPod.PodUID == "" {
		return
	}

	localIP := types.IPToString(key.Family, key.Saddr)
	remoteIP := types.IPToString(key.Family, key.Daddr)
	localPort := key.Sport
	remotePort := key.Dport
	protocol := types.IPProtocolName(key.Protocol)
	direction := directionLabel(key.Direction)

	var srcIP, dstIP string
	var srcPort, dstPort uint16
	var srcNS, srcWorkload, srcPod, srcUID string
	var dstNS, dstUID string

	if key.Direction == 0 { // egress: local=sender
		srcIP, dstIP = localIP, remoteIP
		srcPort, dstPort = localPort, remotePort
		// #197 unconnected UDP TX 는 소켓 이 source 주소 에 bind 되지 않아 BPF 가 saddr 를 unspecified
		// (IPv4 skc_rcv_saddr=0 → "0.0.0.0", IPv6 skc_v6_rcv_saddr=:: → "::") 로 채운다. egress 의 source
		// 는 cgroup 으로 해석 된 local pod 이므로 pod IP 로 backfill 해 실제 소스 를 노출 하고, FlowGuard 의
		// unspecified skip 에 걸리지 않게 한다 (본 backfill 이 Admit 보다 앞서 IPv6 "::" 도 함께 해소).
		if (srcIP == "" || srcIP == "0.0.0.0" || srcIP == "::") && localPod.PodIP != "" {
			srcIP = localPod.PodIP
		}
		srcNS = localPod.NamespaceLabel()
		srcWorkload = localPod.WorkloadLabel()
		srcPod = localPod.PodName
		srcUID = localPod.PodUID
		if c.dstClassifier != nil && c.ip != nil {
			dstIdentity := c.ip.ResolveIP(dstIP)
			dstNS, _, dstUID, _ = c.dstClassifier.Labels(dstIdentity)
		}
	} else { // ingress: local=receiver, src/dst swap so src=sender (remote)
		srcIP, dstIP = remoteIP, localIP
		srcPort, dstPort = remotePort, localPort
		if c.ip != nil {
			srcIdentity := c.ip.ResolveIP(srcIP)
			srcNS = srcIdentity.NamespaceLabel()
			srcWorkload = srcIdentity.WorkloadLabel()
			srcPod = srcIdentity.PodName
			srcUID = srcIdentity.PodUID
		}
		if c.dstClassifier != nil {
			dstNS, _, dstUID, _ = c.dstClassifier.Labels(localPod)
		}
	}

	// FlowGuard 의 namespace 가드 는 local pod 의 namespace 로 검사 한다. swap 과 무관 하게 본 노드
	// 의 allow-list pod 의 양 방향 flow 가 capture 되도록 한다.
	if !c.guard.Admit(localPod.NamespaceLabel(), srcIP, srcPort, dstIP, dstPort, protocol, direction) {
		return
	}

	ak := aggKey{
		srcIP: srcIP, srcPort: srcPort,
		dstIP: dstIP, dstPort: dstPort,
		protocol: protocol, direction: direction,
		ipVersion: types.IPVersion(key.Family),
		dedupeUID: localPod.PodUID,
	}
	if prev, ok := agg[ak]; ok {
		prev.bytes += value.Bytes
		return
	}
	agg[ak] = &aggValue{
		bytes:       value.Bytes,
		srcNS:       srcNS,
		srcWorkload: srcWorkload,
		srcPod:      srcPod,
		srcUID:      srcUID,
		dstNS:       dstNS,
		dstUID:      dstUID,
	}
}

// formatPort 는 uint16 포트 를 라벨 string 으로 변환 한다. strconv.Itoa 는 string allocation 을 동반
// 하나 scrape 주기 와 entry 수 (≤1024) 의 곱이 가벼워 성능 영향 무시 가능 하다.
func formatPort(p uint16) string {
	return uint16ToString(p)
}

// throttledLog 는 last 가 가리키는 unix nano 타임스탬프 기준 으로 errLogMinInterval 안 의 중복 로그를
// 차단 한다. podbytes 와 동일 패턴.
func throttledLog(last *atomic.Int64, format string, args ...any) {
	now := time.Now().UnixNano()
	prev := last.Load()
	if now-prev < int64(errLogMinInterval) {
		return
	}
	if last.CompareAndSwap(prev, now) {
		log.Printf(format, args...)
	}
}
