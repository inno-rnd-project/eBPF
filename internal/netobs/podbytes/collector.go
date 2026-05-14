// Package podbytes는 netobs의 BPF pod_bytes 누적 맵을 Prometheus counter로 노출하는 custom
// collector를 구현한다. 핫 패스에서 패킷당 ringbuf event를 보내는 대신 BPF 측이 (cgroup_id,
// direction, layer) 키로 LRU PERCPU HASH에 bytes/packets를 누적하고, 본 패키지의 collector가
// scrape 시점에 맵을 iterate해 현 누적치를 그대로 emit한다. Prometheus는 monotonically 증가하는
// counter 값을 자연 처리하며 종료된 Pod의 시리즈는 LRU evict 이후 stale로 처리되어 cleanup이
// 자동이다.
package podbytes

import (
	"log"
	"sync/atomic"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/kube"
	ebpfx "netobs/internal/netobs/ebpf"
)

// errLogMinInterval은 silent-skip 에러 로그의 최소 간격이다. scrape는 보통 15-30초 주기로 들어와
// 같은 실패가 분당 수 회 찍히지 않도록 1분 throttle을 둔다.
const errLogMinInterval = time.Minute

// PodResolver는 cgroup_id를 Pod 정체성으로 풀어주는 lookup 인터페이스다. metadata.Enricher가
// 본 인터페이스를 만족하며 단위 테스트에서는 가짜 구현으로 대체 가능하다.
type PodResolver interface {
	ResolveCgroup(cgroupID uint64) (kube.PodIdentity, bool)
}

// direction과 layer 라벨 값 매핑. BPF의 enum 값을 사람이 읽는 라벨 문자열로 변환한다.
// 알 수 없는 값이 들어오면 "unknown"으로 fallback해 카디널리티 안전을 유지한다.
const (
	dirEgress  = "egress"
	dirIngress = "ingress"
	dirUnknown = "unknown"

	layerNIC     = "nic"
	layerL4      = "l4"
	layerUnknown = "unknown"
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

func layerLabel(v uint8) string {
	switch v {
	case 0:
		return layerNIC
	case 1:
		return layerL4
	default:
		return layerUnknown
	}
}

// Collector는 BPF pod_bytes 맵을 scrape 시점에 iterate해 netobs_pod_bytes_total과
// netobs_pod_packets_total를 emit한다. BPF 맵은 ebpfx.Run의 onReady 콜백 이후 SetMap으로
// 주입된다. 주입 전 scrape는 빈 결과만 반환해 startup race를 안전하게 처리한다.
type Collector struct {
	bpfMap   atomic.Pointer[cebpf.Map]
	resolver PodResolver
	node     string
	enabled  bool

	bytesDesc   *prometheus.Desc
	packetsDesc *prometheus.Desc

	// lastCPUErrLogNs / lastIterErrLogNs는 PossibleCPU / iter.Err 실패를 errLogMinInterval 간격으로
	// 로그하기 위한 최근 로그 unix nano 타임스탬프다. 무한 throttle 없이 운영자가 silent skip 원인을
	// 파악할 수 있도록 한다.
	lastCPUErrLogNs  atomic.Int64
	lastIterErrLogNs atomic.Int64
}

// New는 Collector를 구성한다. enabled가 false면 본 Collector는 어떤 시리즈도 emit하지 않으며,
// netobs-agent의 POD_METRICS_ENABLED 토글이 본 매개변수로 그대로 전달된다.
func New(resolver PodResolver, node string, enabled bool) *Collector {
	labels := []string{"direction", "layer", "node", "src_namespace", "src_pod", "src_pod_uid"}
	return &Collector{
		resolver: resolver,
		node:     node,
		enabled:  enabled,
		bytesDesc: prometheus.NewDesc(
			"netobs_pod_bytes_total",
			"Bytes transferred per source pod by direction (egress/ingress) and observation layer (nic/l4)",
			labels, nil,
		),
		packetsDesc: prometheus.NewDesc(
			"netobs_pod_packets_total",
			"Packets transferred per source pod by direction (egress/ingress). Emitted only at layer=nic where one BPF hook call corresponds to one skb",
			labels, nil,
		),
	}
}

// SetMap은 BPF 로드 이후 ebpfx.Runtime이 노출한 PodBytes 맵을 주입한다. 본 호출 전까지 Collect는
// no-op이며 주입 이후부터 실제 시리즈를 emit한다.
func (c *Collector) SetMap(m *cebpf.Map) {
	c.bpfMap.Store(m)
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.bytesDesc
	ch <- c.packetsDesc
}

// throttledLog는 last가 가리키는 unix nano 타임스탬프를 기준으로 errLogMinInterval 안의 중복 로그를
// 차단한다. CAS로 update에 성공한 호출만 실제 로그를 찍어 동시 scrape에서 같은 에러가 다중 출력
// 되지 않는다.
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

// aggKey는 emit 단계의 Prometheus 라벨 셋과 동일 단위로 BPF map entry들을 합산하기 위한 키다.
// BPF는 (cgroup_id, direction, layer) 삼중 키로 누적하지만 한 Pod이 여러 cgroup_id를 가질 수
// 있어 (container 단위, 다중 프로세스 등) 같은 Pod의 entry들이 다중 시리즈로 나타난다. 본 키는
// Pod 단위까지 합쳐 Prometheus의 "동일 라벨 셋 중복 시리즈" 오류를 방지한다.
type aggKey struct {
	direction uint8
	layer     uint8
	podUID    string
}

type aggValue struct {
	bytes   uint64
	packets uint64
	pod     kube.PodIdentity
}

// mergeEntry는 BPF map iteration의 한 entry를 agg 맵에 합산한다. Collect 본문에서 BPF map iterator
// 호출과 PERCPU 슬라이스 길이 처리만 떼어내, 합산/Pod 해상/중복 처리 로직을 단위 테스트로 격리해
// 가드 가능하도록 분리한 헬퍼다. perCPU에는 cilium/ebpf가 LRU_PERCPU_HASH lookup에서 반환한 CPU 수
// 만큼의 슬롯이 들어오고, 모든 슬롯을 합산해 단일 (cgroup_id, direction, layer) entry의 누적치를
// 만든다. resolver가 cgroup_id를 Pod로 해상하지 못한 entry는 skip되며, 같은 PodUID로 해상되는 다른
// cgroup_id entry가 이미 agg에 있으면 동일 (direction, layer, podUID) 키 아래로 합쳐 Prometheus가
// 동일 라벨 셋 중복 시리즈로 거부하는 상황을 방지한다.
func mergeEntry(
	agg map[aggKey]*aggValue,
	key ebpfx.NetObsNetobsPodBytesKey,
	perCPU []ebpfx.NetObsNetobsPodBytesValue,
	resolver PodResolver,
) {
	var totalBytes, totalPackets uint64
	for _, v := range perCPU {
		totalBytes += v.Bytes
		totalPackets += v.Packets
	}

	pod, ok := resolver.ResolveCgroup(key.CgroupId)
	if !ok || !pod.IsPod() {
		return
	}
	// PodUID 빈 문자열 entry는 aggKey가 동일 빈 키로 충돌해 서로 다른 Pod의 bytes/packets가
	// 첫 entry의 라벨 (namespace, podName) 로 새서 나갈 위험이 있다. informer race로 IsPod=true /
	// PodUID="" 가 가능한 짧은 윈도우가 있어 가드한다. 다음 scrape에서 enricher가 UID를 채우면
	// 자연 emit된다.
	if pod.PodUID == "" {
		return
	}

	ak := aggKey{direction: key.Direction, layer: key.Layer, podUID: pod.PodUID}
	if existing, ok := agg[ak]; ok {
		existing.bytes += totalBytes
		existing.packets += totalPackets
		return
	}
	agg[ak] = &aggValue{
		bytes:   totalBytes,
		packets: totalPackets,
		pod:     pod,
	}
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if !c.enabled {
		return
	}
	m := c.bpfMap.Load()
	if m == nil {
		return
	}

	// PERCPU 맵은 lookup마다 CPU 수만큼의 슬롯을 반환한다. values를 미리 PossibleCPUs 길이로
	// 잡아 두면 cilium/ebpf 내부의 reflect 기반 슬라이스 재할당을 entry마다 반복하지 않는다.
	// PossibleCPUs 조회가 실패하면 본 scrape는 안전하게 건너뛴다 (Prometheus는 다음 주기에 재시도).
	ncpus, err := cebpf.PossibleCPU()
	if err != nil {
		throttledLog(&c.lastCPUErrLogNs, "podbytes: PossibleCPU lookup failed, skipping scrape: %v", err)
		return
	}

	var key ebpfx.NetObsNetobsPodBytesKey
	values := make([]ebpfx.NetObsNetobsPodBytesValue, ncpus)

	agg := make(map[aggKey]*aggValue)

	iter := m.Iterate()
	for iter.Next(&key, &values) {
		mergeEntry(agg, key, values, c.resolver)
	}
	// cilium/ebpf 공식 doc: "You must check the result of Err afterwards." 반복 중 ErrIterationAborted
	// 등으로 조기 종료된 경우 부분 결과를 emit하면 Prometheus가 직전 scrape보다 작은 counter 값을
	// 받아 카운터 reset으로 오해할 수 있어, 에러 발생 시 본 scrape는 통째로 폐기한다. EBADF 등
	// shutdown 직후 stale FD 케이스 포함 진단을 위해 throttle 로그를 남긴다.
	if err := iter.Err(); err != nil {
		throttledLog(&c.lastIterErrLogNs, "podbytes: bpf map iterate failed, discarding scrape: %v", err)
		return
	}

	c.emitFromAgg(agg, ch)
}

// emitFromAgg는 합산이 끝난 aggKey/aggValue 맵을 Prometheus metric 채널로 emit한다. Collect 본문의
// emit 단계를 단위 테스트로 분리하기 위한 헬퍼이며 (특히 layer=l4 packets 시리즈 차단 가드의 회귀
// 방지), Collect는 본 메서드를 직접 호출한다.
//
// packets는 NIC layer hook에서만 1 skb = 1 packet 의미가 성립한다 (BPF inc_pod_bytes 주석 참고).
// L4 layer hook (tcp_sendmsg/tcp_cleanup_rbuf) 은 packets_delta=0으로 누적하므로 packets > 0 가드는
// 사실상 layer=l4 entry의 packets 시리즈 emit을 차단하며, 미래에 다른 L4 hook이 추가되어도 의미가
// 깨지지 않게 한다.
func (c *Collector) emitFromAgg(agg map[aggKey]*aggValue, ch chan<- prometheus.Metric) {
	for ak, av := range agg {
		labels := []string{
			directionLabel(ak.direction),
			layerLabel(ak.layer),
			c.node,
			av.pod.Namespace,
			av.pod.PodName,
			av.pod.PodUID,
		}
		ch <- prometheus.MustNewConstMetric(c.bytesDesc, prometheus.CounterValue, float64(av.bytes), labels...)
		if av.packets > 0 {
			ch <- prometheus.MustNewConstMetric(c.packetsDesc, prometheus.CounterValue, float64(av.packets), labels...)
		}
	}
}
