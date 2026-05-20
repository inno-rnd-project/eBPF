// Package metrics 의 tcpstate.go 는 #65 의 receive path 에서 추출한 TCP 혼잡 제어 상태
// (snd_cwnd / srtt_us / snd_ssthresh) 를 수신 Pod 단위로 min/max 집계해 Prometheus gauge 로
// 노출한다. BPF 가 emit 하는 매 packet 단위 sample 은 cardinality 가 폭증하므로 emit 자체는
// Pod 단위 aggregate gauge 로 줄이고 raw 5-tuple 라벨은 노출하지 않는다.
package metrics

import (
	"math"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// tcpInfiniteSsthresh 는 kernel 의 TCP_INFINITE_SSTHRESH 값 (slow start 가 unbounded 인 sentinel) 이다.
// snd_ssthresh 가 본 값이면 ssthresh 가 의미 있는 임계가 아니라 "limit 없음" 표시라 min 집계에서
// 제외해 메트릭이 4G 같은 잡음 값으로 오염되지 않게 한다.
const tcpInfiniteSsthresh uint32 = math.MaxUint32

// TCPStateLabels 는 TCP 상태 메트릭의 emit 라벨 셋이다. receive path 의 ingress event 는 enricher 가
// 흐름 dst 측을 수신 Pod 로 식별하므로 호출자는 그 값을 본 라벨에 채워 전달한다. node 는 agent 가
// 실행 중인 워커 노드를 가리켜 멀티 노드 환경에서 Pod 가 어느 노드에서 관측되었는지 분리한다.
type TCPStateLabels struct {
	Namespace string
	Pod       string
	Node      string
}

type tcpStateAccum struct {
	minCwnd     uint32
	maxSrttUs   uint32
	minSsthresh uint32
	samples     uint64
}

// TCPStateAggregator 는 BPF receive path event 의 TCP 상태 sample 을 Pod 단위로 누적해 scrape
// 시점에 3 종 gauge 로 노출한다. 누적치는 scrape 마다 reset 되어 다음 window 가 직전 결과를
// 끌고 오지 않게 한다. 동시성은 sync.Mutex 로 보호하며 ringbuf consumer (write) 와 prometheus
// scrape (read+reset) 가 직렬화된다.
type TCPStateAggregator struct {
	mu      sync.Mutex
	samples map[TCPStateLabels]*tcpStateAccum

	minCwndDesc     *prometheus.Desc
	maxSrttDesc     *prometheus.Desc
	minSsthreshDesc *prometheus.Desc
}

// NewTCPStateAggregator 는 빈 aggregator 를 생성한다. Prometheus.Collector 인터페이스를 구현하므로
// reg.MustRegister(agg) 한 번으로 3 종 gauge 가 모두 노출된다.
func NewTCPStateAggregator() *TCPStateAggregator {
	labels := []string{"namespace", "pod", "node"}
	return &TCPStateAggregator{
		samples: make(map[TCPStateLabels]*tcpStateAccum),
		minCwndDesc: prometheus.NewDesc(
			"netobs_tcp_state_min_cwnd",
			"Minimum TCP congestion window (segments) observed per receiving Pod during scrape window. Sampled from tcp_sock.snd_cwnd at rcv_demux/rcv_established/rcv_app stages. Resets each scrape.",
			labels, nil,
		),
		maxSrttDesc: prometheus.NewDesc(
			"netobs_tcp_state_max_srtt_seconds",
			"Maximum smoothed round-trip time observed per receiving Pod during scrape window, in seconds. Sampled from tcp_sock.srtt_us (kernel <<3 scale removed in BPF) at receive path stages. Resets each scrape.",
			labels, nil,
		),
		minSsthreshDesc: prometheus.NewDesc(
			"netobs_tcp_state_min_ssthresh",
			"Minimum TCP slow start threshold (segments) observed per receiving Pod during scrape window. tcp_sock.snd_ssthresh values equal to TCP_INFINITE_SSTHRESH are filtered out. Resets each scrape.",
			labels, nil,
		),
	}
}

// Observe 는 receive path event 1 건의 TCP 상태 sample 을 누적한다. cwnd=0 또는 srtt_us=0 같은
// invalid 측정값과 ssthresh=TCP_INFINITE_SSTHRESH sentinel 은 min/max 집계에서 제외해 산출값이
// 잡음에 오염되지 않게 한다. 라벨이 비어 있으면 (namespace="" 등) 수신 Pod 식별 실패로 보고
// emit 자체를 skip 한다.
func (a *TCPStateAggregator) Observe(l TCPStateLabels, cwnd, srttUs, ssthresh uint32) {
	if l.Namespace == "" || l.Pod == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	acc, ok := a.samples[l]
	if !ok {
		acc = &tcpStateAccum{}
		a.samples[l] = acc
	}
	acc.samples++

	if cwnd > 0 && (acc.minCwnd == 0 || cwnd < acc.minCwnd) {
		acc.minCwnd = cwnd
	}
	if srttUs > 0 && srttUs > acc.maxSrttUs {
		acc.maxSrttUs = srttUs
	}
	if ssthresh > 0 && ssthresh != tcpInfiniteSsthresh && (acc.minSsthresh == 0 || ssthresh < acc.minSsthresh) {
		acc.minSsthresh = ssthresh
	}
}

// Describe 는 Prometheus collector 인터페이스 요구사항. 3 종 gauge desc 를 등록한다.
func (a *TCPStateAggregator) Describe(ch chan<- *prometheus.Desc) {
	ch <- a.minCwndDesc
	ch <- a.maxSrttDesc
	ch <- a.minSsthreshDesc
}

// Collect 는 scrape 시점에 누적된 Pod 별 min/max 값을 gauge 로 emit 한 뒤 내부 buffer 를
// reset 한다. 누적치가 0 인 metric (sample 이 없거나 모두 invalid) 은 emit 하지 않아 빈 시계열이
// scrape 결과에 끼지 않게 한다.
func (a *TCPStateAggregator) Collect(ch chan<- prometheus.Metric) {
	a.mu.Lock()
	snapshot := a.samples
	a.samples = make(map[TCPStateLabels]*tcpStateAccum)
	a.mu.Unlock()

	for l, acc := range snapshot {
		if acc.minCwnd > 0 {
			ch <- prometheus.MustNewConstMetric(a.minCwndDesc, prometheus.GaugeValue,
				float64(acc.minCwnd), l.Namespace, l.Pod, l.Node)
		}
		if acc.maxSrttUs > 0 {
			ch <- prometheus.MustNewConstMetric(a.maxSrttDesc, prometheus.GaugeValue,
				float64(acc.maxSrttUs)/1e6, l.Namespace, l.Pod, l.Node)
		}
		if acc.minSsthresh > 0 {
			ch <- prometheus.MustNewConstMetric(a.minSsthreshDesc, prometheus.GaugeValue,
				float64(acc.minSsthresh), l.Namespace, l.Pod, l.Node)
		}
	}
}
