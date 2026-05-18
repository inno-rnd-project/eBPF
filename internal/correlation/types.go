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
type CorrelationResult struct {
	Pair             PairKey         `json:"pair"`
	CorrelationByLag map[int]float64 `json:"correlation_by_lag"`
	MaxAbsLag        int             `json:"max_abs_lag"`
	MaxAbsValue      float64         `json:"max_abs_value"`
	SampleCount      int             `json:"sample_count"`
	Status           Status          `json:"status"`
}
