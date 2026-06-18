package correlation

import "time"

// Config 는 Correlator 가 산출에 사용하는 모든 운영 파라미터를 모은다. zero-config 운영을 위해
// DefaultConfig 가 본 시리즈 (#47 / #48 / #49) 에서 도입된 메트릭들을 default DefaultMetrics 로
// 채워둔다.
type Config struct {
	// PrometheusURL 은 query_range API 의 base URL 이다 (예: http://localhost:9090). PrometheusFetcher
	// 가 본 URL 에 /api/v1/query_range 를 붙여 호출한다.
	PrometheusURL string

	// Window 는 query_range 의 시간 범위다. end = now, start = now - Window 가 적용된다.
	Window time.Duration

	// Step 은 query_range 의 step 이다. window / step 이 표본 수의 최대치다. Prometheus scrape
	// interval (보통 30s) 과 일치시키는 게 자연스럽다.
	Step time.Duration

	// MinSamples 는 Pearson 산출에 요구되는 최소 유효 표본 수다. NaN / Inf pairwise 제거 후의 수가
	// 본 값 미만이면 산출이 skip 된다.
	MinSamples int

	// LagSteps 는 PearsonWithLag 가 산출할 lag step 들이다. 0 은 동시점, +N 은 a 가 b 를 N step
	// 앞서는 관계, -N 은 b 가 a 를 앞서는 관계다. step 의 시간 의미는 Step 과 곱해서 산출된다.
	LagSteps []int

	// DefaultMetrics 는 zero-config 산출 대상 query 들이다. #49 의 cause score 와 latency 가 기본
	// 으로 들어가 운영자가 환경 변수 없이 CLI 만 실행하면 GPU 유휴 진단에 즉시 활용 가능하다.
	DefaultMetrics []string

	// ExtraMetrics 는 운영자가 추가한 query 들이다. DefaultMetrics 에 합쳐져 모든 query 가 동일
	// start / end / step 으로 fetch 된다.
	ExtraMetrics []string

	// FetchTimeout 은 단일 query_range 호출의 HTTP timeout 이다. 24h 윈도우 큰 응답을 받을 때 default
	// 너무 짧으면 timeout 위험이 있어 30s 이상을 권장한다.
	FetchTimeout time.Duration

	// GrangerLag 는 #69 의 Granger causality 산정에 사용하는 lag order p 다. AIC / BIC 자동 선택은
	// 본 시리즈 scope 외이며 호출자가 고정 lag 로 운영한다. 본 시리즈의 기본값은 2 이며 한 단계 lag
	// 의 직접 영향과 두 단계 lag 의 누적 영향까지 잡는다.
	GrangerLag int

	// GrangerMinSamples 는 Granger 산정에서 lag 적용 후 유효 표본 수의 하한이다. 운영 환경의 짧은
	// window (30m / 30s step = 60 samples) 에서도 OK 결과가 나오도록 Pearson 의 MinSamples 와 분리해
	// 더 낮게 둔다. lag p 적용 후 표본 수 n - p 가 본 값 미만이면 GrangerOK=false 로 자연 skip 된다.
	GrangerMinSamples int

	// CrossNodeEnabled 는 #84 의 cross-node interference layer 토글 이다. true 일 때 Correlate 가
	// node 단위 시계열 페어 도 함께 산출 해 결과 슬라이스 에 IsCrossNode=true 항목 으로 append 한다.
	// #147 부터 default true 로 두어 zero-config 에서도 node 단위 간섭 Top-N 이 emit 된다. emit
	// cardinality 는 SelectTopNCrossNode 의 topN 으로, fetch 비용 은 CrossNodeMaxPairs 로 통제 된다.
	// 노드 수 가 매우 많아 부담 인 환경 은 CROSS_NODE=false env 또는 -cross-node=false flag 로 opt-out 한다.
	CrossNodeEnabled bool

	// CrossNodeMaxPairs 는 cross-node 페어 enumerate 의 상한 이다. 노드 수 가 제한적 이라 (dev 4,
	// prod 수십) 실제 cap 발동 케이스 는 거의 없 으나 미래 pod-level cross-node 확장 시 reserved
	// 옵션 으로 활용 가능 하다. 0 이하 면 1024 로 fallback 한다.
	CrossNodeMaxPairs int

	// CrossNodeMetrics 는 #84 의 node 단위 입력 시계열 query 리스트다. DefaultMetrics와 분리되어
	// CrossNodeEnabled=true 일 때만 Correlator.Correlate 가 fetcher 호출 셋에 합류시킨다. opt-in
	// 비활성 운영 모드에서 본 query들이 매 cycle Prometheus 부하를 추가하는 것을 회피한다.
	CrossNodeMetrics []string
}

// DefaultConfig 는 운영자가 zero-config 로 본 라이브러리를 호출할 때 쓰이는 default Config 다.
// DefaultMetrics 는 본 시리즈에서 이미 deploy 된 PrometheusRule recording rule 명을 직접 참조한다.
func DefaultConfig() Config {
	return Config{
		PrometheusURL: "http://localhost:9090",
		Window:        1 * time.Hour,
		Step:          30 * time.Second,
		MinSamples:    60,
		LagSteps:      []int{-1, 0, 1},
		DefaultMetrics: []string{
			// #49 의 pod 단위 cause score 6종. 모두 (node, src_namespace, src_pod, src_pod_uid) 라벨을
			// 보유해 EnumeratePairs 의 pod 페어 schema 와 정합한다.
			"pod:cpu_throttle_score:5m",
			"pod:memory_pressure_score:5m",
			"pod:network_throughput_score:5m",
			"pod:network_retrans_score:5m",
			"pod:host_compute_stall_score:5m",
			// pod:gpu_memory_utilization_ratio:5m 은 gpu_uuid / gpu_index 라벨을 추가로 보유해
			// multi-GPU pod 에서 동일 (namespace, pod) 에 여러 series 가 생성되어 PairKey 중복이 발생
			// 한다. avg by(...) 로 pod 단위 집계해 단일 series 로 normalize 한다.
			`avg by(node, src_namespace, src_pod, src_pod_uid) (pod:gpu_memory_utilization_ratio:5m)`,
			// netobs 의 pod-level latency p99.
			`histogram_quantile(0.99, sum by(node, src_namespace, src_pod, src_pod_uid, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))`,
		},
		FetchTimeout:      30 * time.Second,
		GrangerLag:        2,
		GrangerMinSamples: 30,
		CrossNodeEnabled:  true,
		CrossNodeMaxPairs: 1024,
		// #84 cross-node interference layer 의 node 단위 입력 시계열 5종. CrossNodeEnabled opt-in
		// 시에만 Correlate 가 fetcher 호출 셋에 합류시킨다.
		CrossNodeMetrics: []string{
			"node:cpu_pressure_score:5m",
			"node:memory_pressure_score:5m",
			"node:network_pressure_score:5m",
			"node:gpu_pressure_score:5m",
			"node:netobs_pod_stage_latency_p99:5m",
		},
	}
}
