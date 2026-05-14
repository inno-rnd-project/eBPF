// Package podbytes는 netobs의 BPF pod_bytes 누적 맵을 Prometheus counter로 노출하는 custom
// collector를 구현한다. 핫 패스에서 패킷당 ringbuf event를 보내는 대신 BPF 측이 (cgroup_id,
// direction, layer) 키로 LRU PERCPU HASH에 bytes/packets를 누적하고, 본 패키지의 collector가
// scrape 시점에 맵을 iterate해 현 누적치를 그대로 emit한다. Prometheus는 monotonically 증가하는
// counter 값을 자연 처리하며 종료된 Pod의 시리즈는 LRU evict 이후 stale로 처리되어 cleanup이
// 자동이다.
package podbytes

import (
	"sync/atomic"

	cebpf "github.com/cilium/ebpf"
	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/kube"
	ebpfx "netobs/internal/netobs/ebpf"
)

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
			"Packets transferred per source pod by direction (egress/ingress) and observation layer (nic/l4)",
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
		return
	}

	var key ebpfx.NetObsNetobsPodBytesKey
	values := make([]ebpfx.NetObsNetobsPodBytesValue, ncpus)

	agg := make(map[aggKey]*aggValue)

	iter := m.Iterate()
	for iter.Next(&key, &values) {
		// LRU_PERCPU_HASH는 lookup이 CPU 수만큼의 slice를 반환한다. 모든 CPU의 슬롯을 합산해
		// 단일 (cgroup_id, direction, layer) entry의 누적치를 만든다.
		var totalBytes, totalPackets uint64
		for _, v := range values {
			totalBytes += v.Bytes
			totalPackets += v.Packets
		}

		pod, ok := c.resolver.ResolveCgroup(key.CgroupId)
		if !ok || !pod.IsPod() {
			// cgroup_id가 아직 enricher 캐시에 학습되지 않은 경우 본 entry는 skip한다.
			// event 흐름으로 캐시가 채워지면 다음 scrape에서 정상 emit된다.
			continue
		}

		ak := aggKey{direction: key.Direction, layer: key.Layer, podUID: pod.PodUID}
		if existing, ok := agg[ak]; ok {
			existing.bytes += totalBytes
			existing.packets += totalPackets
			continue
		}
		agg[ak] = &aggValue{
			bytes:   totalBytes,
			packets: totalPackets,
			pod:     pod,
		}
	}
	// cilium/ebpf 공식 doc: "You must check the result of Err afterwards." 반복 중 ErrIterationAborted
	// 등으로 조기 종료된 경우 부분 결과를 emit하면 Prometheus가 직전 scrape보다 작은 counter 값을
	// 받아 카운터 reset으로 오해할 수 있어, 에러 발생 시 본 scrape는 통째로 폐기한다.
	if err := iter.Err(); err != nil {
		return
	}

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
		ch <- prometheus.MustNewConstMetric(c.packetsDesc, prometheus.CounterValue, float64(av.packets), labels...)
	}
}
