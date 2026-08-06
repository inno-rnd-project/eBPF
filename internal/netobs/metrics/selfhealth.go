package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/netobs/types"
)

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

	// eventChannelDepth 는 ringbuf reader 와 이벤트 루프 사이 userspace 채널의 현재 적체 깊이다
	// (#413). 스크레이프 시점의 len(chan) 을 읽는 GaugeFunc 로, ringbuf drop (bpfRingbufDropsTotal)
	// 발생 시 본 값이 용량 근처면 소비자 병목, 0 근처면 커널 버퍼 순간 폭주로 유실 원인을 분리할
	// 수 있다. main 이 채널 생성 후 RegisterEventChannelDepth 로 등록한다.

	// eventProcessingSeconds 는 이벤트 1건의 enrich + Record 처리 wall-clock 분포다 (#413).
	// 버킷은 1µs 부터 약 4s 까지 4배 exponential 12단계로, 통상 수 µs 의 hot path 와 lock 경합 /
	// GC 로 인한 tail 을 함께 커버한다. rate(sum) / rate(count) 와 채널 depth 를 겹쳐 보면 소비
	// 처리량 한계를 정량화할 수 있다.
	eventProcessingSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "netobs_event_processing_seconds",
			Help:    "Wall-clock duration of enrich + record for one ringbuf event in the userspace event loop. Compare with netobs_event_channel_depth and netobs_bpf_ringbuf_drops_total to attribute event loss to consumer throughput vs kernel-side bursts.",
			Buckets: prometheus.ExponentialBuckets(1e-6, 4, 12),
		},
	)

	// flowCounterResetsTotal 은 #416 의 accumulator 맵 evict 실해 신호다. flow_bytes 는 누적 맵이라
	// 점유율이 자연히 1.0 근처에 머무는 것이 정상이고, 실해는 활성 flow 가 LRU 에 밀려나 counter 가
	// 0 부터 다시 쌓이는 reset 이다. flow collector 가 admit 된 (emit 된) 시리즈의 직전 scrape bytes
	// 를 기억해 감소 관측 시 계수하므로 죽은 key 가 채운 점유율과 활성 key 가 밀리는 실해가 구분된다.
	flowCounterResetsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "netobs_flow_counter_resets_total",
			Help: "Number of times an emitted netobs_flow_bytes_total series was observed with a smaller value than the previous scrape, meaning an active flow entry was LRU-evicted from the BPF flow_bytes map and restarted from zero. This is the actionable saturation signal for accumulator maps; utilization ratio alone cannot distinguish dead keys from active-key eviction.",
		},
	)

	// flowGuardRejectedTotal 은 #403 의 emit 상한 계약 카운터다. FlowGuard (스크레이프당 emit
	// budget) 와 DropFlowGuard (라벨셋 sticky 상한) 가 상한 도달로 신규 flow 의 admit 을 거부한
	// 횟수를 guard 라벨로 노출한다. 지속 증가는 max-active 상한이 관측 수요 대비 작다는 신호로,
	// 운영자가 allow-list 를 좁히거나 상한을 올리는 판단 근거가 된다.
	flowGuardRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_flow_guard_rejected_total",
			Help: "Admissions rejected by flow-family cardinality guards after the max-active cap is reached (#403). guard=flow is the per-scrape emit budget of netobs_flow_bytes_total; guard=drop_flow is the sticky label-set cap of netobs_drop_events_flow_total. Sustained growth means the cap is undersized for the observed flow working set.",
		},
		[]string{"guard"},
	)

	// cgroup2Available 은 #297 의 시작 시 statfs 검증 결과다. cgroup v1/hybrid 노드에서 역매핑
	// 스캐너가 조용히 빈 테이블로 degrade 하던 것을 운영자가 식별할 수 있게 한다.
	cgroup2Available = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "netobs_cgroup2_available",
			Help: "1 if the host cgroup root is a cgroup2 filesystem (statfs magic checked once at startup), 0 otherwise. When 0 the cgroup-id reverse-mapping scanner (#228) is disabled and UDP-only pod attribution falls back to ringbuf hint learning only.",
		},
	)

	// podNoSockets 는 #342 의 pod 별 소켓 존재 스캔 결과다. netns 소켓 테이블 (tcp/tcp6/udp/udp6)
	// 이 전부 빈 pod 만 1 로 노출한다. TCP/UDP 이벤트가 구조적으로 없어 netobs 시리즈 부재가 관측
	// 결함이 아님을 증명하는 시리즈로, correlation 의 미관측 사유 no_traffic 판별 입력이 된다.
	// hostNetwork pod 는 netns 공유로 판별이 무의미해 emit 되지 않는다. 스캔마다 Reset 후 재설정
	// 되어 소켓이 생긴 pod 의 시리즈는 자연 소거된다.
	podNoSockets = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "netobs_pod_no_sockets",
			Help: "1 if the pod's network namespace currently has zero tcp/tcp6/udp/udp6 sockets (checked from /proc/<pid>/net on the cgroup scanner cycle). Such pods structurally produce no netobs series, so their absence is classified as no_traffic instead of no_data. hostNetwork pods are excluded because they share the host netns.",
		},
		[]string{"node", "src_namespace", "src_pod"},
	)

	// socketScanDurationSeconds 와 socketScanPods 는 #342 소켓 스캔의 self-health 다. 스캔 비용
	// (마지막 스캔 소요 시간) 과 판별에 성공한 pod 수를 노출해 cgroup.procs / procfs 읽기 비용이
	// 노드 pod 수에 비례해 커지는 상황을 운영자가 관찰할 수 있게 한다.
	socketScanDurationSeconds = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "netobs_socket_scan_duration_seconds",
			Help: "Wall-clock duration of the last per-pod socket presence scan piggybacking on the cgroup scanner cycle.",
		},
	)
	socketScanPods = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "netobs_socket_scan_pods",
			Help: "Number of pods whose socket presence was successfully determined in the last scan. Pods skipped for hostNetwork, missing PIDs (terminated), or procfs read failures are not counted.",
		},
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

	// bpfProgramAttachTotal 은 #105 의 BPF program attach 시도 누적 카운터다. startup 단계 에서 program
	// 별 attach 호출마다 result=success 또는 result=failure 로 emit 된다. counter 라 agent restart 시
	// 0 으로 리셋 되며, 본 동작은 "본 에이전트 인스턴스 의 attach 시도 빈도" 운영 의미와 자연 정합 한다
	// (bpfRingbufDropsTotal 의 baseline-then-delta 패턴과 의도적 으로 다른 lifecycle).
	bpfProgramAttachTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_bpf_program_attach_total",
			Help: "Cumulative attach attempts per BPF program (kprobe / kretprobe), partitioned by result. result=success counts attempts that completed within the retry budget; result=failure counts attempts that exhausted retries. Unlike netobs_bpf_program_loaded (current state gauge), this counter visualizes attach attempt frequency since agent startup. Reset on agent restart.",
		},
		[]string{"program", "result"},
	)

	// bpfProgramAttachRetryTotal 은 #105 의 attach retry 부담 누적 카운터다. attach 시도 한 번 안에서
	// retry 가 발생할 때마다 reason 라벨 (#105 의 7종 enum) 과 함께 +1 emit 된다. result=success 이지만
	// retry_total 이 누적된 program 은 transient flap 으로 식별 가능 하고, retry_total 누적 후 결국
	// result=failure 로 마감된 program 은 영구 실패로 분류된다 (program_loaded=0 동시 관측).
	bpfProgramAttachRetryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_bpf_program_attach_retry_total",
			Help: "Cumulative attach retries per BPF program, partitioned by classified failure reason (#105 7-value enum: symbol_not_found, kernel_version_mismatch, btf_missing, verifier_rejected, permission_denied, link_internal_error, other). Each retry increments the counter; the program that eventually succeeds shows up as transient flap (success in netobs_bpf_program_attach_total + retry_total > 0).",
		},
		[]string{"program", "reason"},
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

// RegisterEventChannelDepth 는 이벤트 채널의 len 을 읽는 GaugeFunc 를 등록한다. 채널은 main 이
// 소유하고 수명이 프로세스와 같아 closure capture 가 안전하다. len(chan) 은 락 없는 근사 스냅샷
// 이라 스크레이프 hot path 에 무해하다.
func RegisterEventChannelDepth(reg prometheus.Registerer, ch chan types.Event, capacity int) {
	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name:        "netobs_event_channel_depth",
			Help:        "Current number of events buffered in the userspace channel between the ringbuf reader and the event loop. Values near the capacity indicate a consumer bottleneck.",
			ConstLabels: prometheus.Labels{"capacity": strconv.Itoa(capacity)},
		},
		func() float64 { return float64(len(ch)) },
	))
}

// ObserveEventProcessing 은 이벤트 1건의 처리 소요를 히스토그램에 기록한다.
func ObserveEventProcessing(d time.Duration) {
	eventProcessingSeconds.Observe(d.Seconds())
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

// AddFlowCounterResets 는 flow collector 가 한 scrape 에서 관측한 counter reset 수를 누적한다 (#416).
func AddFlowCounterResets(n uint64) {
	if n > 0 {
		flowCounterResetsTotal.Add(float64(n))
	}
}

// AddFlowGuardRejected 는 flow 계열 가드의 상한 거부 1 회를 기록한다 (#403). guard 는 "flow"
// (FlowGuard 의 스크레이프당 emit budget) 또는 "drop_flow" (DropFlowGuard 의 sticky 상한) 다.
func AddFlowGuardRejected(guard string) {
	flowGuardRejectedTotal.WithLabelValues(guard).Inc()
}

// PreregisterFlowGuardLabels 는 flow 계열 가드의 rejected 카운터 라벨을 0 으로 선등록해 거부가
// 아직 없는 시점에도 시리즈가 존재하게 한다 (#403). increase() 기반 관측이 첫 거부 전에도 0 을
// 읽을 수 있다.
func PreregisterFlowGuardLabels() {
	flowGuardRejectedTotal.WithLabelValues("flow")
	flowGuardRejectedTotal.WithLabelValues("drop_flow")
}

// SetBpfMapUtilization 은 map 라벨 별 포화 비율을 emit 한다. ratio 는 호출 측에서 current /
// max_entries 로 정규화한 [0, 1] 값을 전달한다.
func SetBpfMapUtilization(mapName string, ratio float64) {
	bpfMapUtilizationRatio.WithLabelValues(mapName).Set(ratio)
}

// SetCgroup2Available 은 host cgroup root 의 cgroup2 여부를 1/0 으로 노출한다 (#297). cgroup id
// 역매핑 스캐너는 "cgroup id == 디렉터리 inode" 동일성 (cgroup2 전제) 에 의존하므로, 0 이면 UDP
// 전용 pod 의 cgroup 귀속이 힌트 캐시에 한정되는 degrade 상태다.
func SetCgroup2Available(ok bool) {
	v := 0.0
	if ok {
		v = 1
	}
	cgroup2Available.Set(v)
}

// SetSocketScan 은 #342 소켓 존재 스캔 결과를 emit 한다. 무소켓 pod 게이지는 스캔마다 Reset 후
// 재설정되어 stale 시리즈가 남지 않고, self-health 2 종 (소요 시간과 판별 pod 수) 을 함께 갱신한다.
// socketless 는 (namespace, pod) 쌍의 평탄화 슬라이스다.
func SetSocketScan(node string, socketless [][2]string, scanned int, durationSeconds float64) {
	podNoSockets.Reset()
	for _, np := range socketless {
		podNoSockets.WithLabelValues(node, np[0], np[1]).Set(1)
	}
	socketScanDurationSeconds.Set(durationSeconds)
	socketScanPods.Set(float64(scanned))
}

// SetInformerSyncLag 는 informer watch event 수신 lag 을 초 단위로 emit 한다. 호출 측이 last
// event time 0 (미수신) 케이스의 fallback 처리를 수행해 본 함수는 항상 의미 있는 수치만 받는다.
func SetInformerSyncLag(seconds float64) {
	informerSyncLagSeconds.Set(seconds)
}

// RecordBpfAttachResult 는 #105 의 attach 시도 결과 emit 진입점이다. 한 번의 attach 시도 (retry 포함)
// 가 success 로 마감 됐는지 budget 소진 후 failure 로 끝났는지를 program 라벨 과 함께 누적 한다.
// loader 는 retry loop 마감 시점 에 본 helper 를 1 회 호출 해 result 라벨 의 누적 의미를 attempts-per-program
// 으로 일관 유지 한다. 매 retry 시점 의 시도 횟수 는 별도 RecordBpfAttachRetry 에서 추적 한다.
func RecordBpfAttachResult(program string, success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	bpfProgramAttachTotal.WithLabelValues(program, result).Inc()
}

// RecordBpfAttachRetry 는 #105 의 retry 부담 emit 진입점이다. attach 호출 1 회의 매 retry 시도 마다
// classifyAttachError 분류 결과의 String() 출력 을 reason 라벨 로 전달 받아 +1 누적 한다. reason 라벨
// 값은 ebpf 패키지의 AttachReason.String() 결과 7종 enum 으로 폐쇄 유지 되어 카디널리티 폭증 위험이
// 없다. metrics 패키지가 ebpf 패키지를 import 하면 ebpf → metrics → ebpf import cycle 이 발생 하므로
// caller (loader) 가 string 으로 변환 후 본 helper 에 전달 하는 의존 방향 단방향 invariant 를 유지 한다.
func RecordBpfAttachRetry(program, reason string) {
	bpfProgramAttachRetryTotal.WithLabelValues(program, reason).Inc()
}

// PreregisterBpfAttachLabels 는 #105 의 attach 메트릭 카디널리티 사전 등록 진입점이다. agent startup
// 단계 에서 tracked program 셋 과 reason enum 라벨 값 의 모든 조합 을 0 으로 노출 해 시리즈 발생 전
// 에도 dashboard / alert query 가 빈 결과 가 아닌 0 값 을 받도록 한다. reasons 슬라이스 는 caller (loader)
// 가 ebpf.AttachReasonValues 를 String() 으로 변환 후 전달 한다.
func PreregisterBpfAttachLabels(programs, reasons []string) {
	for _, p := range programs {
		// counter 의 WithLabelValues 호출 만으로 시리즈 가 생성 되며 초기값 은 0 이다. Inc 호출 없음.
		bpfProgramAttachTotal.WithLabelValues(p, "success")
		bpfProgramAttachTotal.WithLabelValues(p, "failure")
		for _, r := range reasons {
			bpfProgramAttachRetryTotal.WithLabelValues(p, r)
		}
	}
}
