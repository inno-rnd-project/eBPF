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
		FetchTimeout: 30 * time.Second,
		GrangerLag:   2,
	}
}
