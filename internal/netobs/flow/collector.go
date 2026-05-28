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
	bpfMap         atomic.Pointer[cebpf.Map]
	cgroup         CgroupResolver
	ip             IPResolver
	guard          *metrics.FlowGuard
	dstClassifier  *metadata.DstLabelClassifier
	node           string
	enabled        bool

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

// Collect 는 BPF flow_bytes 맵 을 iterate 해 FlowGuard 통과 entry 를 netobs_flow_bytes_total 로 emit
// 한다. guard 의 allow-list 가 비어 있어 어떤 namespace 도 admit 되지 않는 경우 iterate 자체 를 skip
// 해 BPF map iteration 비용 을 0 으로 유지 한다.
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

	iter := m.Iterate()
	for iter.Next(&key, &value) {
		c.emitEntry(ch, key, value)
	}
	// cilium/ebpf 의 NextKey 가 ErrIterationAborted 등으로 조기 종료 되면 부분 결과 가 Prometheus 의
	// counter reset 으로 오해 될 수 있어 본 scrape 전체 폐기 + throttled log.
	if err := iter.Err(); err != nil {
		throttledLog(&c.lastIterErrLogNs, "flow: bpf map iterate failed, discarding partial scrape: %v", err)
	}
}

// emitEntry 는 단일 BPF map entry 를 라벨 변환 과 FlowGuard 검증 후 메트릭 으로 변환 한다. Collect 의
// hot loop 에서 호출 되며 가드 실패 entry 는 즉시 skip 한다.
func (c *Collector) emitEntry(ch chan<- prometheus.Metric, key ebpfx.NetObsNetobsFlowKey, value ebpfx.NetObsNetobsFlowValue) {
	pod, ok := c.cgroup.ResolveCgroup(key.CgroupId)
	if !ok || !pod.IsPod() || pod.PodUID == "" {
		return
	}

	srcIP := types.U32ToIPv4(key.Saddr)
	dstIP := types.U32ToIPv4(key.Daddr)
	protocol := types.IPProtocolName(key.Protocol)
	direction := directionLabel(key.Direction)

	if !c.guard.Admit(pod.NamespaceLabel(), srcIP, key.Sport, dstIP, key.Dport, protocol, direction) {
		return
	}

	var dstNS, dstUID string
	if c.dstClassifier != nil && c.ip != nil {
		dstIdentity := c.ip.ResolveIP(dstIP)
		dstNS, _, dstUID, _ = c.dstClassifier.Labels(dstIdentity)
	}

	ch <- prometheus.MustNewConstMetric(
		c.bytesDesc,
		prometheus.CounterValue,
		float64(value.Bytes),
		c.node,
		pod.NamespaceLabel(), pod.WorkloadLabel(), pod.PodName, pod.PodUID,
		srcIP, formatPort(key.Sport),
		dstNS, dstUID,
		dstIP, formatPort(key.Dport),
		protocol,
		direction,
	)
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
