// Package podbytes는 netobs의 BPF pod_bytes 누적 맵을 Prometheus counter로 노출하는 custom
// collector를 구현한다. 핫 패스에서 패킷당 ringbuf event를 보내는 대신 BPF 측이 (cgroup_id,
// direction, layer) 키로 LRU PERCPU HASH에 bytes/packets를 누적하고, 본 패키지의 collector가
// scrape 시점에 맵을 iterate해 현 누적치를 그대로 emit한다. Prometheus는 monotonically 증가하는
// counter 값을 자연 처리하며 종료된 Pod의 시리즈는 LRU evict 이후 stale로 처리되어 cleanup이
// 자동이다.
package podbytes

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

// errLogMinInterval은 silent-skip 에러 로그의 최소 간격이다. scrape는 보통 15-30초 주기로 들어와
// 같은 실패가 분당 수 회 찍히지 않도록 1분 throttle을 둔다.
const errLogMinInterval = time.Minute

// PodResolver는 cgroup_id를 Pod 정체성으로 풀어주는 lookup 인터페이스다. metadata.Enricher가
// 본 인터페이스를 만족하며 단위 테스트에서는 가짜 구현으로 대체 가능하다.
type PodResolver interface {
	ResolveCgroup(cgroupID uint64) (kube.PodIdentity, bool)
	// ResolveCgroupScanned는 cgroup 스캐너 테이블로만 푼다. 스캐너가 없으면 항상 miss다.
	ResolveCgroupScanned(cgroupID uint64) (kube.PodIdentity, bool)
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

	// known은 한 번이라도 스캐너가 pod로 푼 cgroup_id다. 컨테이너가 재시작하면 옛 cgroup이 더는 풀리지
	// 않는데, 그 누적분을 빼면 pod 합계가 줄어 Prometheus가 counter reset으로 읽는다. 같은 pod의
	// 다른 cgroup이 살아 있는 동안은 옛 cgroup의 누적분을 합계에 유지한다. mu가 보호한다.
	mu    sync.Mutex
	known map[uint64]kube.PodIdentity

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
		known:    make(map[uint64]kube.PodIdentity),
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

// mapEntry는 BPF 맵 iterate 결과 한 건이다. bytes와 packets는 PERCPU 슬롯을 모두 더한 값이다.
type mapEntry struct {
	key     ebpfx.NetObsNetobsPodBytesKey
	bytes   uint64
	packets uint64
}

func newMapEntry(key ebpfx.NetObsNetobsPodBytesKey, perCPU []ebpfx.NetObsNetobsPodBytesValue) mapEntry {
	e := mapEntry{key: key}
	for _, v := range perCPU {
		e.bytes += v.Bytes
		e.packets += v.Packets
	}
	return e
}

func addEntry(agg map[aggKey]*aggValue, e mapEntry, pod kube.PodIdentity) {
	ak := aggKey{direction: e.key.Direction, layer: e.key.Layer, podUID: pod.PodUID}
	if existing, ok := agg[ak]; ok {
		existing.bytes += e.bytes
		existing.packets += e.packets
		return
	}
	agg[ak] = &aggValue{bytes: e.bytes, packets: e.packets, pod: pod}
}

// aggregate는 entry를 pod 단위로 합산한다. 호출자가 mu를 잡아야 한다. pod로 풀린 entry는 바로 합산하고,
// 스캐너가 푼 cgroup이면 known에 기억한다. UID가 빈 pod(informer race의 짧은 창)는 서로 다른 pod가 같은
// 빈 키로 합쳐지므로 풀리지 않은 것으로 본다. 풀리지 않은 entry는 known에 있고 그 pod가 이번 scrape에서
// 다른 cgroup으로 살아 있음이 확인될 때만 합산한다. 컨테이너가 재시작해도 pod 합계가 줄지 않고, pod가
// 사라지면 모든 cgroup이 풀리지 않아 시리즈가 감소 없이 사라진다. 스캐너가 없는 구성에서 힌트로만 풀린
// cgroup은 기억하지 않는다. 힌트는 추론이라 틀릴 수 있고, 기억하면 틀린 귀속이 힌트 만료 뒤에도 남는다.
func (c *Collector) aggregate(entries []mapEntry) map[aggKey]*aggValue {
	agg := make(map[aggKey]*aggValue)
	live := make(map[string]struct{})
	inMap := make(map[uint64]struct{}, len(entries))
	var unresolved []mapEntry
	for _, e := range entries {
		inMap[e.key.CgroupId] = struct{}{}
		pod, ok := c.resolver.ResolveCgroup(e.key.CgroupId)
		if !ok || !pod.IsPod() || pod.PodUID == "" {
			unresolved = append(unresolved, e)
			continue
		}
		if _, scanned := c.resolver.ResolveCgroupScanned(e.key.CgroupId); scanned {
			c.known[e.key.CgroupId] = pod
		}
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

	var entries []mapEntry

	iter := m.Iterate()
	for iter.Next(&key, &values) {
		entries = append(entries, newMapEntry(key, values))
	}
	// cilium/ebpf 공식 doc: "You must check the result of Err afterwards." 반복 중 ErrIterationAborted
	// 등으로 조기 종료된 경우 부분 결과를 emit하면 Prometheus가 직전 scrape보다 작은 counter 값을
	// 받아 카운터 reset으로 오해할 수 있어, 에러 발생 시 본 scrape는 통째로 폐기한다. EBADF 등
	// shutdown 직후 stale FD 케이스 포함 진단을 위해 throttle 로그를 남긴다.
	if err := iter.Err(); err != nil {
		throttledLog(&c.lastIterErrLogNs, "podbytes: bpf map iterate failed, discarding scrape: %v", err)
		return
	}

	c.mu.Lock()
	agg := c.aggregate(entries)
	c.mu.Unlock()
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
