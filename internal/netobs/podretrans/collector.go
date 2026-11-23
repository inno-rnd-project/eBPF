// Package podretrans는 netobs의 BPF pod_retrans 누적 맵을 pod별 TCP 재전송 세그먼트와 바이트 counter로
// 노출하는 custom collector다. BPF 측이 tcp_retransmit_skb와 tcp_retransmit_synack raw tracepoint에서
// 성공한 재전송을 (소켓 cgroup_id, kind) 키로 누적하고, 이 collector가 scrape 시점에 맵을 iterate해
// pod 단위로 합산한 누적치를 emit한다. kprobe 이벤트 기반 netobs_retrans_events_labeled_total이 호출
// 단위라 GSO 세그먼트 수와 TLP 재전송을 놓치는 것과 달리, kind 합계가 pod netns의 Tcp RetransSegs에서
// TcpExt TCPRetransFail을 뺀 값과 같다.
//
// cgroup 귀속은 cgroup 역매핑 스캐너 테이블만 쓴다. 힌트 캐시는 IP나 ifindex로 추론한 identity에서도
// 학습되어 호스트 소켓(sshd 등)의 재전송이 pod로 계상될 수 있기 때문이다. 스캐너가 모르는 cgroup은
// 호스트 프로세스, 아직 스캔되지 않은 신규 pod, 재시작이나 종료로 scope가 사라진 컨테이너 중 하나다.
// 신규 pod는 BPF 맵이 계속 누적하므로 다음 스캔 이후 누적치 그대로 나타난다. 사라진 컨테이너의
// entry를 그냥 빼면 sidecar가 남은 pod의 합계가 줄어 Prometheus가 counter reset으로 읽으므로, 한 번
// 풀린 cgroup의 pod를 기억해 두고 같은 pod의 다른 cgroup이 아직 풀리는 동안은 합계에 계속 넣는다.
package podretrans

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/kube"
	ebpfx "netobs/internal/netobs/ebpf"
)

// errLogMinInterval은 iterate 실패 로그의 최소 간격이다. podbytes collector와 같은 1분 throttle이다.
const errLogMinInterval = time.Minute

// PodResolver는 소켓 cgroup_id를 pod 정체성으로 풀어주는 lookup 인터페이스다. metadata.Enricher의
// ResolveCgroupScanned가 이를 만족하며 단위 테스트에서는 가짜 구현으로 대체한다.
type PodResolver interface {
	ResolveCgroupScanned(cgroupID uint64) (kube.PodIdentity, bool)
}

// kind 라벨 값. BPF enum netobs_retrans_kind와 같은 순서이며 알 수 없는 값은 unknown으로 둔다.
const (
	kindData    = "data"
	kindSyn     = "syn"
	kindSynack  = "synack"
	kindUnknown = "unknown"

	bpfKindSynack = 2
)

func kindLabel(v uint8) string {
	switch v {
	case 0:
		return kindData
	case 1:
		return kindSyn
	case bpfKindSynack:
		return kindSynack
	default:
		return kindUnknown
	}
}

// Collector는 BPF pod_retrans 맵을 scrape 시점에 iterate해 netobs_pod_retrans_segments_total과
// netobs_pod_retrans_bytes_total을 emit한다. 맵은 ebpfx.Run의 onReady 이후 SetMap으로 주입되며, 주입
// 전 scrape와 enabled=false(POD_METRICS_ENABLED 토글)는 빈 결과를 반환한다.
type Collector struct {
	bpfMap   atomic.Pointer[cebpf.Map]
	resolver PodResolver
	node     string
	enabled  bool

	segsDesc  *prometheus.Desc
	bytesDesc *prometheus.Desc

	lastIterErrLogNs atomic.Int64

	// known은 스캐너로 한 번이라도 풀린 cgroup의 pod 정체성이다. 동시 scrape가 Collect를 겹쳐 부를 수
	// 있어 mu로 보호하며, BPF 맵에서 사라진 cgroup은 매 scrape 끝에 지워 크기가 맵 크기를 넘지 않는다.
	mu    sync.Mutex
	known map[uint64]kube.PodIdentity
}

// New는 Collector를 구성한다. enabled에는 netobs-agent의 POD_METRICS_ENABLED 토글이 그대로 전달된다.
func New(resolver PodResolver, node string, enabled bool) *Collector {
	labels := []string{"kind", "node", "src_namespace", "src_pod", "src_pod_uid"}
	return &Collector{
		resolver: resolver,
		node:     node,
		enabled:  enabled,
		known:    make(map[uint64]kube.PodIdentity),
		segsDesc: prometheus.NewDesc(
			"netobs_pod_retrans_segments_total",
			"TCP segments retransmitted per source pod, counted at the tcp_retransmit_skb and tcp_retransmit_synack tracepoints for successful retransmissions only (tcp_skb_pcount). kind=data covers RTO, fast retransmit and tail loss probe, kind=syn is client SYN, kind=synack is listener SYN-ACK. For a pod with its own network namespace, the sum over kind equals that namespace's Tcp RetransSegs minus TcpExt TCPRetransFail; hostNetwork pods share the host namespace, so their series cover only their own sockets. Unlike netobs_retrans_events_labeled_total, which counts kprobe calls, this counts segments",
			labels, nil,
		),
		bytesDesc: prometheus.NewDesc(
			"netobs_pod_retrans_bytes_total",
			"TCP payload bytes retransmitted per source pod (skb->len at the tcp_retransmit_skb tracepoint, successful retransmissions only). Equals the socket tcpi_bytes_retrans when TcpExt TCPRetransFail does not change. SYN-ACK retransmissions carry no payload and are not emitted",
			labels, nil,
		),
	}
}

// SetMap은 BPF 로드 이후 ebpfx.Runtime이 노출한 PodRetrans 맵을 주입한다. runner 종료 시 nil로 되돌려
// 닫힌 FD를 iterate하지 않게 한다.
func (c *Collector) SetMap(m *cebpf.Map) {
	c.bpfMap.Store(m)
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.segsDesc
	ch <- c.bytesDesc
}

// aggKey는 emit 라벨 셋과 같은 단위다. 한 pod가 container별로 여러 cgroup_id를 가지므로 BPF entry를
// pod UID 단위로 합쳐 Prometheus의 동일 라벨 셋 중복 시리즈를 막는다.
type aggKey struct {
	kind   uint8
	podUID string
}

type aggValue struct {
	segs  uint64
	bytes uint64
	pod   kube.PodIdentity
}

// mapEntry는 BPF 맵 iterate 결과 한 건이다.
type mapEntry struct {
	key ebpfx.NetObsNetobsPodRetransKey
	val ebpfx.NetObsNetobsPodRetransValue
}

func addEntry(agg map[aggKey]*aggValue, e mapEntry, pod kube.PodIdentity) {
	ak := aggKey{kind: e.key.Kind, podUID: pod.PodUID}
	if existing, ok := agg[ak]; ok {
		existing.segs += e.val.Segs
		existing.bytes += e.val.Bytes
		return
	}
	agg[ak] = &aggValue{segs: e.val.Segs, bytes: e.val.Bytes, pod: pod}
}

// aggregate는 entry를 pod 단위로 합산한다. 호출자가 mu를 잡아야 한다. 스캐너가 pod로 푼 entry는 바로
// 합산하고 그 cgroup을 known에 기억한다. UID가 빈 pod는 서로 다른 pod가 같은 빈 키로 합쳐지므로
// 풀리지 않은 것으로 본다. 풀리지 않은 entry는 known에 있고 그 pod가 이번 scrape에서 다른 cgroup으로
// 살아 있음이 확인될 때만 합산한다. 컨테이너가 재시작해도 pod 합계가 줄지 않고, pod가 사라지면 모든
// cgroup이 풀리지 않아 시리즈가 감소 없이 사라진다.
func (c *Collector) aggregate(entries []mapEntry) map[aggKey]*aggValue {
	agg := make(map[aggKey]*aggValue)
	live := make(map[string]struct{})
	inMap := make(map[uint64]struct{}, len(entries))
	var unresolved []mapEntry
	for _, e := range entries {
		inMap[e.key.CgroupId] = struct{}{}
		pod, ok := c.resolver.ResolveCgroupScanned(e.key.CgroupId)
		if !ok || !pod.IsPod() || pod.PodUID == "" {
			unresolved = append(unresolved, e)
			continue
		}
		c.known[e.key.CgroupId] = pod
		live[pod.PodUID] = struct{}{}
		addEntry(agg, e, pod)
	}
	for _, e := range unresolved {
		pod, ok := c.known[e.key.CgroupId]
		if !ok {
			continue
		}
		if _, alive := live[pod.PodUID]; alive {
			addEntry(agg, e, pod)
		}
	}
	for cg := range c.known {
		if _, ok := inMap[cg]; !ok {
			delete(c.known, cg)
		}
	}
	return agg
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	if !c.enabled {
		return
	}
	m := c.bpfMap.Load()
	if m == nil {
		return
	}

	var e mapEntry
	var entries []mapEntry
	iter := m.Iterate()
	for iter.Next(&e.key, &e.val) {
		entries = append(entries, e)
	}
	// 부분 결과를 emit하면 직전 scrape보다 작은 counter가 나가 reset으로 오해되므로 통째로 버린다.
	if err := iter.Err(); err != nil {
		now := time.Now().UnixNano()
		if prev := c.lastIterErrLogNs.Load(); now-prev >= int64(errLogMinInterval) && c.lastIterErrLogNs.CompareAndSwap(prev, now) {
			log.Printf("podretrans: bpf map iterate failed, discarding scrape: %v", err)
		}
		return
	}
	c.mu.Lock()
	agg := c.aggregate(entries)
	c.mu.Unlock()
	c.emitFromAgg(agg, ch)
}

// emitFromAgg는 합산 결과를 emit한다. SYN-ACK은 payload가 없어 bytes 시리즈를 내지 않는다.
func (c *Collector) emitFromAgg(agg map[aggKey]*aggValue, ch chan<- prometheus.Metric) {
	for ak, av := range agg {
		labels := []string{kindLabel(ak.kind), c.node, av.pod.Namespace, av.pod.PodName, av.pod.PodUID}
		ch <- prometheus.MustNewConstMetric(c.segsDesc, prometheus.CounterValue, float64(av.segs), labels...)
		if ak.kind != bpfKindSynack {
			ch <- prometheus.MustNewConstMetric(c.bytesDesc, prometheus.CounterValue, float64(av.bytes), labels...)
		}
	}
}
