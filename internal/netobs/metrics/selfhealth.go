package metrics

import "github.com/prometheus/client_golang/prometheus"

// self-health 메트릭은 netobs agent 자체의 내부 상태를 노출한다. correlation-exporter 와 workload-
// injector 가 *_reconcile_*, *_runs_total, *_errors_total 패턴으로 컴포넌트 건강성을 가시화하는
// 것과 동등한 자리이며, BPF map 포화와 informer sync lag 같은 agent 운영 신호가 본 자리에서 합쳐진다.
// 본 파일의 4 메트릭은 dst_classifier_emits_total 과 같은 self-observe 카테고리에 속한다.
var (
	// bpfProgramLoaded 는 kprobe / kretprobe attach 결과를 심볼 단위로 노출한다. required attach 가
	// 실패하면 agent 가 즉시 crash 해 본 메트릭 emit 자체가 도달 불가능하지만, optional attach 가
	// kernel 버전 또는 빌드 옵션 차이로 silent skip 된 경우 운영자가 본 게이지로 어느 심볼이 비었는지
	// 진단할 수 있다. gpuobs_cuda_symbol_available (#41) 과 동일한 1=loaded / 0=missing 의미다.
	bpfProgramLoaded = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "netobs_bpf_program_loaded",
			Help: "1 if the BPF kprobe / kretprobe for the symbol is currently attached, 0 if attach was attempted but failed (typically optional probes on kernels without the symbol). Required probes that crash the agent on failure are not emitted here; use up{job=\"netobs-agent\"} for that layer.",
		},
		[]string{"symbol"},
	)

	// bpfRingbufDropsTotal 은 events ringbuf 의 reserve 실패 누적이다. BPF 측 percpu events_dropped
	// 카운터를 userspace refresher 가 주기적으로 read 와 sum 해 baseline-then-delta 로 본 counter 에
	// Add 한다. agent 재시작 시 카운터는 0 으로 재시작되어 Prometheus 의 counter reset 규약과 정합한다.
	bpfRingbufDropsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "netobs_bpf_ringbuf_drops_total",
			Help: "Cumulative number of events that the BPF program failed to reserve in the events ringbuf because the buffer was full. Sampled from a BPF percpu counter every refresh cycle via baseline-then-delta. A non-zero rate indicates the userspace ringbuf reader is falling behind producers.",
		},
	)

	// bpfMapUtilizationRatio 는 BPF map 의 current entries / max entries 비율이다. starts 와
	// pod_bytes 두 LRU 계열 map 의 포화 신호를 운영자에게 노출한다. LRU evict 가 동작해 운영상 1.0
	// 도달은 통상적으로 일어나지 않지만, 0.8 을 안정적으로 넘으면 max_entries 가 워크로드 규모를
	// 따라가지 못한다는 신호이며 #70 의 NetObsBpfMapUtilizationHigh alert 가 본 신호에 묶여 있다.
	bpfMapUtilizationRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "netobs_bpf_map_utilization_ratio",
			Help: "Ratio of current entries to max_entries for tracked BPF maps. Sampled every refresh cycle by iterating the map. Useful as an early warning before LRU evict pressure starts surfacing as visible data loss.",
		},
		[]string{"map"},
	)

	// informerSyncLagSeconds 는 kube.Resolver 의 마지막 watch event 수신 후 경과 시간이다. 첫
	// 이벤트 수신 전 (agent startup 직후) 윈도우에서는 agent 기동 시각으로부터의 경과 시간으로
	// fallback 해 startup 단계에서도 의미 있는 신호를 노출한다. API server 단절이나 RBAC 실수로
	// watch 가 끊긴 케이스가 본 게이지의 단조 증가로 가시화된다.
	informerSyncLagSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "netobs_informer_sync_lag_seconds",
			Help: "Seconds since the kube informer last received any watch event for Pod / Service / Node. Before the first event the gauge falls back to seconds since agent startup so it remains interpretable during the warm-up window. Stale informer cache is detected by sustained values well above the resync period.",
		},
	)
)

// SetBpfProgramLoaded 는 kprobe / kretprobe attach 결과를 심볼 단위로 emit 한다. loader 가 attach
// 시도 전에 모든 트래킹 심볼을 0 으로 선등록한 뒤 성공한 심볼만 1 로 올리는 패턴을 사용해, agent
// 가 attach 단계조차 도달하지 못한 경우에도 0 값이 그대로 노출되도록 한다.
func SetBpfProgramLoaded(symbol string, loaded bool) {
	v := 0.0
	if loaded {
		v = 1.0
	}
	bpfProgramLoaded.WithLabelValues(symbol).Set(v)
}

// AddBpfRingbufDrops 는 BPF percpu events_dropped 의 baseline-then-delta 산정 결과를 누적한다.
// refresher 가 reset 케이스 (current < last) 에서는 본 함수를 호출하지 않고 baseline 만 갱신해
// 거짓 spike 를 회피한다.
func AddBpfRingbufDrops(delta uint64) {
	if delta == 0 {
		return
	}
	bpfRingbufDropsTotal.Add(float64(delta))
}

// SetBpfMapUtilization 은 map 라벨 별 포화 비율을 emit 한다. ratio 는 호출 측에서 current /
// max_entries 로 정규화한 [0, 1] 값을 전달한다.
func SetBpfMapUtilization(mapName string, ratio float64) {
	bpfMapUtilizationRatio.WithLabelValues(mapName).Set(ratio)
}

// SetInformerSyncLag 는 informer watch event 수신 lag 을 초 단위로 emit 한다. 호출 측이 last
// event time 0 (미수신) 케이스의 fallback 처리를 수행해 본 함수는 항상 의미 있는 수치만 받는다.
func SetInformerSyncLag(seconds float64) {
	informerSyncLagSeconds.Set(seconds)
}
