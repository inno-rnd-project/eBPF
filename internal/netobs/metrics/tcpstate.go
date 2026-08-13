// Package metrics 의 tcpstate.go 는 #65 의 receive path 에서 추출한 TCP 혼잡 제어 상태
// (snd_cwnd / srtt_us / snd_ssthresh) 를 수신 Pod 단위로 min/max 집계해 Prometheus gauge 로
// 노출한다. BPF 가 emit 하는 매 packet 단위 sample 은 cardinality 가 폭증하므로 emit 자체는
// Pod 단위 aggregate gauge 로 줄이고 raw 5-tuple 라벨은 노출하지 않는다.
package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// tcpInfiniteSsthresh 는 kernel 의 TCP_INFINITE_SSTHRESH 값 (slow start 가 unbounded 인 sentinel) 이다.
// include/net/tcp.h 의 정의는 0x7FFFFFFF = INT_MAX 다 (uint32 max 가 아니다). snd_ssthresh 가 본 값
// 이면 ssthresh 가 의미 있는 임계가 아니라 "limit 없음" 표시라 min 집계에서 제외해 메트릭이 2G 같은
// 잡음 값으로 오염되지 않게 한다.
const tcpInfiniteSsthresh uint32 = 0x7FFFFFFF

// tcpStateWindow는 min/max 집계창의 길이다. #443부터 Collect가 비파괴라 창 회전은 Observe가
// 수행하며, scrape 간격(30s)의 2배로 두어 어떤 scrape 시점에도 직전 30~60초의 실측이 남아 있게
// 한다. 창이 만료되면 다음 Observe가 해당 pod의 누적치를 새 창으로 리셋한다.
const tcpStateWindow = 60 * time.Second

// tcpStateIdleTTL은 sample이 끊긴 entry의 보존 기한이다. 종전 파괴적 리셋은 죽은 pod entry를
// scrape마다 비웠지만 비파괴 전환으로 명시 프루닝이 필요해졌다. Collect가 lastSeen 기준으로
// 만료 entry를 삭제해 종료된 pod의 시리즈가 마지막 값으로 무기한 잔류하지 않게 한다.
const tcpStateIdleTTL = 5 * time.Minute

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
	windowStart time.Time
	lastSeen    time.Time
}

// TCPStateAggregator 는 BPF receive path event 의 TCP 상태 sample 을 Pod 단위로 누적해 scrape
// 시점에 3 종 gauge 로 노출한다. #443부터 Collect는 비파괴 읽기다. 종전 scrape마다의 파괴적
// 리셋은 Prometheus 본 scrape 외의 추가 reader(진단 curl, 이중 scraper)가 값을 소거해 정규
// 시계열이 결측되는 결함이 있었다. 창 회전은 Observe가 tcpStateWindow 기준으로 수행하고, 죽은
// pod 정리는 Collect의 tcpStateIdleTTL 프루닝이 담당한다. 동시성은 sync.Mutex 로 보호하며
// ringbuf consumer (write) 와 prometheus scrape (read+prune) 가 직렬화된다.
type TCPStateAggregator struct {
	mu      sync.Mutex
	samples map[TCPStateLabels]*tcpStateAccum
	// now는 시간 주입점이다. 창 회전과 idle 프루닝의 단위 테스트가 실제 시계 없이 경과를
	// 시뮬레이션할 수 있게 하고, 운영 경로는 time.Now 그대로다.
	now func() time.Time

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
		now:     time.Now,
		minCwndDesc: prometheus.NewDesc(
			"netobs_tcp_state_min_cwnd",
			"Minimum TCP congestion window (segments) observed per receiving Pod over a rotating 60s window. Sampled from tcp_sock.snd_cwnd at rcv_demux/rcv_established/rcv_app stages.",
			labels, nil,
		),
		maxSrttDesc: prometheus.NewDesc(
			"netobs_tcp_state_max_srtt_seconds",
			"Maximum smoothed round-trip time observed per receiving Pod over a rotating 60s window, in seconds. Sampled from tcp_sock.srtt_us (kernel <<3 scale removed in BPF) at receive path stages.",
			labels, nil,
		),
		minSsthreshDesc: prometheus.NewDesc(
			"netobs_tcp_state_min_ssthresh",
			"Minimum TCP slow start threshold (segments) observed per receiving Pod over a rotating 60s window. tcp_sock.snd_ssthresh values equal to TCP_INFINITE_SSTHRESH are filtered out.",
			labels, nil,
		),
	}
}

// Observe 는 receive path event 1 건의 TCP 상태 sample 을 누적한다. cwnd=0 또는 srtt_us=0 같은
// invalid 측정값과 ssthresh=TCP_INFINITE_SSTHRESH sentinel 은 min/max 집계에서 제외해 산출값이
// 잡음에 오염되지 않게 한다. 라벨이 비어 있으면 (namespace="" 등) 수신 Pod 식별 실패로 보고
// emit 자체를 skip 한다. 해당 pod의 집계창(tcpStateWindow)이 만료됐으면 누적치를 새 창으로
// 리셋한 뒤 반영한다(#443 창 회전).
func (a *TCPStateAggregator) Observe(l TCPStateLabels, cwnd, srttUs, ssthresh uint32) {
	if l.Namespace == "" || l.Pod == "" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	acc, ok := a.samples[l]
	if !ok {
		acc = &tcpStateAccum{windowStart: now}
		a.samples[l] = acc
	} else if now.Sub(acc.windowStart) >= tcpStateWindow {
		*acc = tcpStateAccum{windowStart: now}
	}
	acc.lastSeen = now
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

// Collect 는 scrape 시점에 누적된 Pod 별 min/max 값을 gauge 로 emit 한다. 비파괴 읽기라 연속
// scrape가 같은 창의 값을 재관측할 수 있다(#443). sample이 tcpStateIdleTTL 이상 끊긴 entry는
// 여기서 삭제해 종료된 pod의 시리즈가 잔류하지 않게 한다. 누적치가 0 인 metric (sample 이 없거나
// 모두 invalid) 은 emit 하지 않아 빈 시계열이 scrape 결과에 끼지 않게 한다.
func (a *TCPStateAggregator) Collect(ch chan<- prometheus.Metric) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	for l, acc := range a.samples {
		if now.Sub(acc.lastSeen) >= tcpStateIdleTTL {
			delete(a.samples, l)
			continue
		}
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
