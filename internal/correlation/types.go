// Package correlation은 Prometheus query_range API로 가져온 netobs와 gpuobs 시계열의 Pearson 상관
// 계수를 산출하는 stateless 라이브러리다. 데이터 수집 파이프라인 (DaemonSet agent → Prometheus
// scrape → TSDB) 과 분리된 후행 분석 layer로 동작하며 운영자가 cmd/correlation-debug CLI로 1회성
// 호출하는 형태가 본 이슈 #50의 expose 범위다. 주기적 자동화 (exporter / CronJob) 는 #51에서 다룬다.
//
// 본 패키지의 책임은 다음 네 가지다.
//  1. Prometheus /api/v1/query_range로 다중 메트릭의 시계열을 가져오기 (fetcher)
//  2. 노드 내 Pod 페어를 enumerate해 cross-product 폭발을 통제 (pair)
//  3. 각 페어의 Pearson 상관계수를 lag 0 / +1 / -1 세 시점에서 산출 후 최대 절대값 채택 (pearson)
//  4. 결과를 CorrelationResult slice로 반환 (orchestrator)
//
// 모든 산출은 호출 단위 stateless다. 시계열 buffer는 함수 scope 내 임시 자료로만 존재하고 GC된다.
// 영구 저장소를 두지 않으며 cluster에 새 워크로드를 추가하지 않는다.
package correlation

// Sample은 단일 시점의 (timestamp, value) 쌍이다. timestamp는 Prometheus query_range가 반환하는
// epoch milliseconds를 그대로 보존해 두 시계열의 timestamp 정렬을 비교 가능하게 한다.
type Sample struct {
	TimestampMs int64   `json:"timestamp_ms"`
	Value       float64 `json:"value"`
}

// TimeSeries는 라벨 셋과 그 라벨에 해당하는 시계열 샘플들이다. Labels는 Prometheus가 반환한 원본
// 라벨을 그대로 보존해 pair enumeration이 (node, src_namespace, src_pod, src_pod_uid) 같은 키로
// 동일성을 판정할 수 있게 한다. Samples는 query_range step 단위로 정렬된 상태로 들어온다.
type TimeSeries struct {
	Labels  map[string]string `json:"labels"`
	Samples []Sample          `json:"samples"`
}

// PairKey는 상관계수 산출 대상 페어의 정체성을 표현하는 키다. src와 dst는 비대칭이며 운영자가
// (X, Y) 결과와 (Y, X) 결과를 별도로 해석 가능하다. Pod UID 는 namespace + pod 이름이 재사용되는
// StatefulSet 환경에서 재생성 전후의 시리즈를 구분하고 downstream consumer (#51 exporter) 가 UID
// 라벨을 그대로 사용할 수 있게 한다.
type PairKey struct {
	SrcNamespace string `json:"src_namespace"`
	SrcPod       string `json:"src_pod"`
	SrcPodUID    string `json:"src_pod_uid"`
	SrcMetric    string `json:"src_metric"`
	DstNamespace string `json:"dst_namespace"`
	DstPod       string `json:"dst_pod"`
	DstPodUID    string `json:"dst_pod_uid"`
	DstMetric    string `json:"dst_metric"`
}

// Status는 단일 페어 산출 결과의 분류다. exporter (#51) 가 메트릭 라벨로 본 값을 그대로 사용 가능
// 하도록 string enum으로 둔다.
type Status string

const (
	// StatusOK는 유효한 표본 수와 분산으로 산출이 성공한 상태다.
	StatusOK Status = "ok"
	// StatusSkippedLowSamples는 입력 표본 수가 minSamples 미만이라 산출을 건너뛴 상태다. value는 0.
	StatusSkippedLowSamples Status = "skipped_low_samples"
	// StatusSkippedConstant는 두 시계열 중 하나라도 분산이 0 (상수 시계열) 이라 산출을 건너뛴 상태다.
	// Pearson 정의상 분모가 0이 되어 NaN인데 본 패키지는 NaN 대신 0과 본 status를 반환한다.
	StatusSkippedConstant Status = "skipped_constant"
	// StatusPartial은 일부 lag에서는 산출됐고 일부 lag에서는 표본 부족으로 누락된 상태다. 최대 절대값
	// 채택은 산출 가능한 lag 범위 내에서만 수행된다.
	StatusPartial Status = "partial"
)

// CorrelationResult는 단일 페어의 lag별 상관계수와 최대 절대값 채택 결과를 담는다. #51 exporter가
// 본 struct를 그대로 입력으로 받아 Prometheus 메트릭으로 변환 가능하도록 JSON tag 셋을 동결한다.
// #69 의 Granger causality 산정 결과 (FStatistic, PValue, GrangerOK) 도 본 구조체로 함께 전달된다.
// #84 의 cross-node interference layer 가 같은 슬라이스에 결과를 함께 반환하므로 IsCrossNode 플래그
// 와 NodePair 가 추가 첨부되어 caller 가 pod-level 결과와 node-level 결과를 단일 키로 분기 식별 가능
// 하다. IsCrossNode=false 일 때 Pair 가, IsCrossNode=true 일 때 NodePair 가 유효하다.
type CorrelationResult struct {
	Pair        PairKey     `json:"pair"`
	NodePair    NodePairKey `json:"node_pair,omitempty"`
	IsCrossNode bool        `json:"is_cross_node,omitempty"`
	// MaxAbsSignedValue 는 채택 lag (MaxAbsLag) 의 부호 있는 상관값이다 (#406). 종전의 lag 별 전체
	// map (CorrelationByLag) 은 소비자가 SelectTopN 방향 게이트의 채택 lag 부호 조회 1곳뿐인데
	// 페어마다 map 을 힙 할당해 대형 클러스터에서 페어 수만큼의 불필요한 할당원이었다. |corr| 는
	// MaxAbsValue 가, 부호는 본 필드가 담당한다.
	MaxAbsSignedValue float64 `json:"max_abs_signed_value"`
	MaxAbsLag         int     `json:"max_abs_lag"`
	MaxAbsValue       float64 `json:"max_abs_value"`
	SampleCount       int     `json:"sample_count"`
	Status            Status  `json:"status"`
	// FStatistic 과 PValue 는 #69 의 Granger causality 산정 결과다. src 가 dst 를 Granger-cause 하는지
	// 의 통계적 유의성을 노출한다. GrangerOK 가 false 면 표본 부족 또는 행렬 singular 로 산정이 자연
	// skip 된 상태이며 FStatistic 과 PValue 모두 0 으로 둔다.
	FStatistic float64 `json:"f_statistic"`
	PValue     float64 `json:"p_value"`
	GrangerOK  bool    `json:"granger_ok"`
	// Impact 와 ImpactOK 는 #146 의 effect size 산정 결과다. Impact 는 suspect 압박 구간과 비압박
	// 구간의 victim latency 차이 (seconds) 로 간섭의 절대 영향 크기다. ImpactOK 가 false 면 표본
	// 부족 또는 suspect 상수로 산정이 자연 skip 된 상태이며 Impact 는 0 으로 둔다. #175 부터 본 두
	// 필드는 latency victim 전용 legacy 로 유지되고 (impact_seconds 단위 정합), 전 신호 확장은 아래
	// ImpactMagnitude 가 담당한다.
	Impact   float64 `json:"impact_seconds"`
	ImpactOK bool    `json:"impact_ok"`
	// #175 ImpactMagnitude 는 victim 신호별 native 단위의 degradation 크기 (latency=seconds 증가,
	// throughput=bytes/s 감소, error=drops/s 증가, gpu=util 감소) 로 latency 외 신호까지 확장된 effect
	// size 다. impact_seconds 는 latency 전용 legacy alias 이며 신규 소비자는 본 필드와 victim_signal
	// 라벨로 신호별 단위를 해석한다. ImpactPValue 는 high / low 구간 평균 차이의 Welch t-test two-sided
	// p-value 로 effect size 의 통계적 유의성이다. *OK 가 false 면 표본 부족이나 구간 분산 0 등으로
	// 산정이 graceful skip 된 상태이며 값은 0 으로 둔다.
	ImpactMagnitude   float64 `json:"impact_magnitude"`
	ImpactMagnitudeOK bool    `json:"impact_magnitude_ok"`
	ImpactPValue      float64 `json:"impact_pvalue"`
	ImpactPValueOK    bool    `json:"impact_pvalue_ok"`
	// ServiceImpactPair 와 IsServiceImpact 는 #148 의 service-impact layer (workload 단위 victim) 결과다.
	// IsCrossNode 와 동일한 분기 마킹 패턴으로 caller 가 pod / node / workload 세 입도를 단일 슬라이스
	// 에서 식별 가능하다. IsServiceImpact=true 일 때만 ServiceImpactPair 가 유효하며 이때 Pair 와
	// NodePair 는 비어 있다.
	ServiceImpactPair ServiceImpactPairKey `json:"service_impact_pair,omitempty"`
	IsServiceImpact   bool                 `json:"is_service_impact,omitempty"`
	// CrossLevelPair 와 IsCrossLevel 는 #149 의 cross-granularity layer (동일 node 내 node 압박과 pod
	// latency 를 잇는) 결과다. 위 세 분기와 동일한 마킹 패턴으로 caller 가 네 입도 (pod / node /
	// workload / cross-level) 를 단일 슬라이스에서 식별 가능하다. IsCrossLevel=true 일 때만 CrossLevelPair
	// 가 유효하며 이때 나머지 Pair 필드는 비어 있다.
	CrossLevelPair CrossLevelPairKey `json:"cross_level_pair,omitempty"`
	IsCrossLevel   bool              `json:"is_cross_level,omitempty"`
}
