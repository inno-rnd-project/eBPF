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

	// #353 GrangerLag (고정 lag order) 는 제거됐다. Granger 검정은 Pearson 이 선택한 lag
	// (CorrelationResult.MaxAbsLag) 에서 산정해 lag_seconds 와 pvalue 가 동일 lag 을 가리키게 한다.
	// 고정 lag 은 Pearson 선택 lag 과 어긋나 인과 유의성이 보고된 lag 을 뒷받침하지 못했다.

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
	// CrossNodeEnabled=true 일 때 Correlator.Correlate 가 fetcher 호출 셋에 합류시킨다. #147 부터
	// default 활성 이며 CROSS_NODE=false / -cross-node=false 로 opt-out 하면 본 query들의 매 cycle
	// Prometheus 부하를 회피한다.
	CrossNodeMetrics []string

	// ServiceImpactEnabled 는 #148 의 service-impact layer 토글이다. true 일 때 Correlate 가 suspect
	// node 자원 압박과 victim workload (Service 근사) latency 페어도 함께 산출해 결과 슬라이스에
	// IsServiceImpact=true 항목으로 append 한다. #148 부터 default true 로 두어 zero-config 에서도
	// service 단위 영향 Top-N 이 emit 된다. 노드 / workload 수가 매우 많아 부담인 환경은
	// SERVICE_IMPACT=false env 또는 -service-impact=false flag 로 opt-out 한다.
	ServiceImpactEnabled bool

	// ServiceImpactMaxPairs 는 service-impact 페어 enumerate 의 상한이다. 노드 수 * pressure dimension
	// 수 * workload 수라 노드 수가 제한적인 환경에서는 cap 발동이 드물다. 0 이하면 4096 으로 fallback
	// 한다.
	ServiceImpactMaxPairs int

	// ServiceImpactMetrics 는 #148 의 service-impact 입력 시계열 query 리스트다. suspect 는 node 단위
	// 압박 score 4종, victim 은 workload 단위 p99 latency 1종이다. CrossNodeMetrics 의 node 압박 score
	// 와 query 문자열이 겹치면 PlannedQueries 가 dedup 해 중복 fetch 를 회피하며, CrossNodeEnabled 와
	// 무관하게 본 layer 가 자체적으로 suspect 입력을 확보하도록 node 압박 score 를 포함한다.
	ServiceImpactMetrics []string

	// CrossLevelEnabled 는 #149 의 cross-granularity layer 토글이다. true 일 때 Correlate 가 동일 node
	// 안에서 node 압박과 pod latency 를 잇는 양방향 (node_to_pod / pod_to_node) 페어를 추가 산출해
	// 결과 슬라이스에 IsCrossLevel=true 항목으로 append 한다. #149 부터 default true 로 두어 zero-config
	// 에서도 cross-level 영향 Top-N 이 emit 된다. 입력은 pod 압박/latency (DefaultMetrics) 와 node
	// 압박/latency (CrossNodeMetrics) 를 그대로 재사용하므로 새 query 가 없다. 카디널리티 부담 환경은
	// CROSS_LEVEL=false env 또는 -cross-level=false flag 로 opt-out 한다.
	CrossLevelEnabled bool

	// CrossLevelMaxPairs 는 cross-level 페어 enumerate 의 상한이다. node 압박 dimension 수 * 동일 node
	// pod 수 * 두 방향이라 pod 수가 많은 대형 cluster 에서 폭증할 수 있어 본 캡으로 트림한다. 0 이하면
	// 4096 으로 fallback 한다. CrossLevelAllowNamespaces 와 함께 카디널리티를 통제한다.
	CrossLevelMaxPairs int

	// CrossLevelAllowNamespaces 는 cross-level 페어에 참여할 pod 의 src_namespace allow-list 다. 비어
	// 있으면 모든 namespace 를 허용하고 CrossLevelMaxPairs 캡이 backstop 이 된다. 운영자가 특정
	// namespace (예: latency-sensitive app) 로 좁혀 페어 수를 줄이는 카디널리티 통제 수단이다.
	CrossLevelAllowNamespaces []string

	// ImpactGraphEnabled 는 #151 Phase 1 의 영향 전파 그래프 토글이다. true 일 때 exporter 가 매 reconcile
	// cycle 의 noisy neighbor Top-N 을 정점 (pod) 과 방향 엣지 (suspect → victim) 로 하는 in-memory
	// 그래프로 구성해 REST API 와 node degree 메트릭으로 노출한다. 같은 토글이 켜져 있으면 Phase 2 의
	// 다단계 경로 추출 (ExtractImpactPaths) 도 함께 수행된다. 새 Prometheus fetch 없이 기존 Top-N 을
	// 재사용하므로 비용이 작다. #151 부터 default true 이며 IMPACT_GRAPH=false env 또는
	// -impact-graph=false flag 로 opt-out 한다.
	ImpactGraphEnabled bool

	// ImpactPathMaxDepth 는 #151 Phase 2 다단계 경로 추출의 최대 hop 수다. 깊은 경로는 의미가 흐려지고
	// 조밀 그래프에서 경로 수가 폭증하므로 본 값으로 제한한다. 0 이하면 5 로 fallback 한다.
	ImpactPathMaxDepth int

	// ImpactPathMinScore 는 경로 엣지로 인정할 최소 상관 score 다. 이 미만 엣지는 약한 전파로 보고
	// 가지치기해 경로의 의미와 cardinality 를 통제한다.
	ImpactPathMinScore float64

	// ImpactPathMaxPaths 는 추출 경로 수의 상한이다. 조밀 그래프의 조합 폭발을 방어하는 backstop 이며
	// 0 이하면 1024 로 fallback 한다.
	ImpactPathMaxPaths int

	// MinSuspectScore 는 #245 의 무부하 노이즈 게이트다. suspect (cause score) 시계열의 window 내
	// 최대 절대값이 본 값 미만이면 페어 생성 전에 시리즈를 제거해, 근제로 노이즈 간 파형 유사성이
	// 강한 상관으로 산출되는 것을 차단한다. suspect 는 모두 0-1 정규화 score 라 단일 임계가 정합하며
	// victim 시계열 (native 단위) 은 게이트 대상이 아니다. 0 이하면 비활성이다.
	MinSuspectScore float64
}

// PlannedQueries 는 활성 layer 를 반영해 Correlate 가 fetch 할 query 의 dedup 합집합을 반환한다.
// DefaultMetrics 와 ExtraMetrics 에 더해 CrossNodeEnabled / ServiceImpactEnabled 토글에 따라 각 layer
// 의 입력 query 를 합치되, layer 간 동일 query (예: node 압박 score 는 cross-node 와 service-impact 가
// 공유) 는 한 번만 남겨 중복 fetch 를 회피한다. exporter 의 self-health (expected vs observed metric
// 수) 가 본 dedup 후 count 와 정합해야 ReconcilePartial 이 거짓 증가하지 않으므로 fetch 측과 expected
// 측이 본 함수를 공유한다.
func (c Config) PlannedQueries() []string {
	out := make([]string, 0, len(c.DefaultMetrics)+len(c.ExtraMetrics)+len(c.CrossNodeMetrics)+len(c.ServiceImpactMetrics))
	seen := make(map[string]struct{}, cap(out))
	add := func(qs []string) {
		for _, q := range qs {
			if _, ok := seen[q]; ok {
				continue
			}
			seen[q] = struct{}{}
			out = append(out, q)
		}
	}
	add(c.DefaultMetrics)
	add(c.ExtraMetrics)
	// CrossNodeMetrics (node 압박 + node latency) 는 cross-node 뿐 아니라 cross-level (#149) 의 입력
	// 이기도 하다. CrossNodeEnabled 가 false 라도 CrossLevelEnabled 면 node 입도 시계열을 fetch 해야
	// 동일 node 의 node↔pod 페어가 성립하므로 둘 중 하나라도 활성이면 합류시킨다.
	if c.CrossNodeEnabled || c.CrossLevelEnabled {
		add(c.CrossNodeMetrics)
	}
	if c.ServiceImpactEnabled {
		add(c.ServiceImpactMetrics)
	}
	return out
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
			// netobs 의 pod-level latency p99 victim (#150 victim_signal=latency).
			`histogram_quantile(0.99, sum by(node, src_namespace, src_pod, src_pod_uid, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))`,
			// #150 throughput victim (victim_signal=throughput). netobs_pod_bytes_total 의 egress nic
			// 바이트 rate 를 pod 단위로 집계한다. 간섭으로 throughput 이 저하되면 suspect 압박과 음의 상관
			// 으로 나타나며 SelectTopN 은 max|corr| 로 부호 무관하게 포착한다. "bytes" 토큰이라 classify
			// VictimSignal 이 throughput 으로 분류한다.
			`sum by(node, src_namespace, src_pod, src_pod_uid) (rate(netobs_pod_bytes_total{direction="egress",layer="nic"}[5m]))`,
			// #150 error victim (victim_signal=error). pod 단위 drop rate 다. netobs_drop_events_flow_total
			// 은 src_pod 를 보유하나 NETOBS_DROP_FLOW_ALLOW_NAMESPACES allow-list 에 등록된 namespace 에서만
			// emit 되어 미설정 시 본 victim 은 graceful 하게 비어 있다. classifyVictimSignal 이 source 메트릭
			// 이름 netobs_drop_events_flow_total 로 매칭해 error 로 분류한다. 본 메트릭은 src_pod_uid 라벨을
			// 보유하지 않아 sum by 에 넣어도 PromQL 이 결과에서 제거하므로 (no-op) 의도적으로 제외했고, victim
			// pod_uid 는 빈 값이라 SelectTopN 이 (namespace, pod) fallback 으로 dedup / 그룹화한다.
			`sum by(node, src_namespace, src_pod) (rate(netobs_drop_events_flow_total[5m]))`,
			// #174 GPU victim (victim_signal=gpu). pod 단위 GPU 사용률 p95 다. 네트워크 간섭으로 GPU
			// 워크로드가 starvation 되면 사용률이 떨어져 suspect 압박과 음의 상관으로 나타나며 SelectTopN
			// 은 max|corr| 로 부호 무관하게 포착한다. pod:gpu_util_p95:5m 은 gpu_uuid 라벨을 추가로 보유해
			// multi-GPU pod 에서 동일 (namespace, pod) 에 여러 series 가 생성되므로 avg by(...) 로 pod 단위
			// 집계해 단일 series 로 normalize 한다 (suspect pod:gpu_memory_utilization_ratio:5m 과 동일
			// 패턴). 기존 GPU suspect 와 동일하게 GPU 워크로드 의 per-pod 귀속 (gpuobs_pod_utilization_percent)
			// 이 있어야 emit 되며 미귀속 시 graceful 하게 비어 있다. classifyVictimSignal 이 pod:gpu_util 토큰
			// 으로 매칭해 gpu 로 분류한다.
			`avg by(node, src_namespace, src_pod, src_pod_uid) (pod:gpu_util_p95:5m)`,
		},
		FetchTimeout:      30 * time.Second,
		GrangerMinSamples: 30,
		CrossNodeEnabled:  true,
		CrossNodeMaxPairs: 1024,
		// #84 cross-node interference layer 의 node 단위 입력 시계열 5종. CrossNodeEnabled=true (#147
		// 부터 default 활성) 일 때 Correlate 가 fetcher 호출 셋에 합류시킨다.
		CrossNodeMetrics: []string{
			"node:cpu_pressure_score:5m",
			"node:memory_pressure_score:5m",
			"node:network_pressure_score:5m",
			"node:gpu_pressure_score:5m",
			"node:netobs_pod_stage_latency_p99:5m",
		},
		ServiceImpactEnabled:  true,
		ServiceImpactMaxPairs: 4096,
		// #148 service-impact layer 의 입력 시계열. suspect 는 node 압박 score 4종 (cross-node 와 공유,
		// PlannedQueries 가 dedup), victim 은 workload 단위 p99 latency 1종이다. workload:netobs_stage_
		// latency_p99:5m 은 netobs 의 src_workload (owner) 라벨로 집계되어 service 단위 latency 를 근사
		// 한다. CrossNodeEnabled=true (#147 부터 default) 일 때 Correlate 가 fetcher 호출 셋에 합류시킨다.
		ServiceImpactMetrics: []string{
			"node:cpu_pressure_score:5m",
			"node:memory_pressure_score:5m",
			"node:network_pressure_score:5m",
			"node:gpu_pressure_score:5m",
			"workload:netobs_stage_latency_p99:5m",
		},
		// #149 cross-level layer 는 새 입력 query 가 없다. pod 압박/latency 는 DefaultMetrics, node
		// 압박/latency 는 CrossNodeMetrics 를 그대로 재사용한다. allow-list 는 기본 비워 모든 namespace
		// 를 허용하고 MaxPairs 캡을 backstop 으로 둔다.
		CrossLevelEnabled:         true,
		CrossLevelMaxPairs:        4096,
		CrossLevelAllowNamespaces: nil,
		// #151 Phase 1 영향 전파 그래프. 기존 noisy neighbor Top-N 을 재사용해 새 입력 query 가 없다.
		ImpactGraphEnabled: true,
		// #151 Phase 2 다단계 경로 추출 기본값. max_depth 5 로 깊이를 제한하고 min_score 0.5 로 약한
		// 엣지를 가지치기하며 max_paths 1024 를 조합 폭발 backstop 으로 둔다.
		ImpactPathMaxDepth: 5,
		ImpactPathMinScore: 0.5,
		MinSuspectScore:    0.1,
		ImpactPathMaxPaths: 1024,
	}
}
