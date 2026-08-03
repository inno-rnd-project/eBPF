package correlation

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"netobs/internal/correlation/granger"
)

// Correlator 는 fetcher 로 시계열을 가져와 pair enumerate 후 Pearson 으로 상관계수를 산출하는
// orchestrator 다. 상태 없이 매 호출이 독립적이라 동일 인스턴스가 여러 호출 간 reuse 가능하다.
type Correlator struct {
	fetcher Fetcher
	config  Config
}

// New 는 Fetcher 와 Config 로 Correlator 를 구성한다. Fetcher 인터페이스를 받아 단위 테스트에서
// mock 으로 대체 가능하다.
func New(fetcher Fetcher, config Config) *Correlator {
	return &Correlator{fetcher: fetcher, config: config}
}

// Config 는 본 Correlator 의 Config 사본을 반환한다. exporter 가 reconcile cycle 결과를 self-health
// 메트릭에 반영할 때 expected query 수 같은 메타데이터를 라이브러리 외부에서 참조하기 위한 진입점.
func (c *Correlator) Config() Config { return c.config }

// Correlate 는 endTime 을 기준으로 [endTime-Window, endTime] 범위의 산출을 수행한다. endTime 을
// 호출자가 명시하므로 함수 자체는 결정적이며 (time.Now() 의존성 없음) 단위 테스트와 과거 시점
// 분석 (예: alert 발화 시점 기준 회귀 분석) 모두 가능하다. 운영자가 \"지금 기준\" 을 의도하면
// time.Now() 를 호출 인자로 명시한다 (CLI 의 기본 동작).
//
// 다음 순서로 산출을 수행한다.
//
//  1. config 의 DefaultMetrics 와 ExtraMetrics 를 합쳐 모든 query 를 동일 start / end / step 으로
//     병렬 fetch
//  2. 모든 LabeledSeries 를 EnumeratePairs 로 노드 한정 페어로 변환
//  3. 각 페어를 PearsonWithLag 로 lag 별 산출
//  4. 산출 결과를 []CorrelationResult 로 반환
//
// 일부 query 가 fetch 실패해도 나머지로 산출을 계속한다. 모든 query 가 실패할 때만 wrapped error 를
// 반환한다. fetch 결과가 비어 있는 (시계열 0개) query 는 정상으로 보고 페어 산출에서 자연 제외된다.
// per-query 실패 내역이 필요한 호출자 (exporter 의 self-health, #405) 는 CorrelateWithStats 를 쓴다.
func (c *Correlator) Correlate(ctx context.Context, endTime time.Time) ([]CorrelationResult, error) {
	results, _, err := c.CorrelateWithStats(ctx, endTime)
	return results, err
}

// FetchStats 는 한 cycle 의 per-query fetch 결과와 페어 절단 요약이다 (#405, #406). FailedQueries
// 는 실패한 query 문자열 목록으로, 호출자가 per-query 카운터와 로그에 쓴다. 종전에는 부분 실패가
// 로그도 카운터도 없이 성공 경로에 흡수되어 fetch 16개 중 15개가 실패해도 침묵했다. TruncatedPairs
// 는 레이어별 (pod / cross_node / service_impact / cross_level) maxPairs 캡으로 잘려 나간 페어 수로,
// 캡이 발동하지 않은 레이어는 항목이 없다. exporter 가 correlation_pairs_truncated_total{layer}
// 카운터로 노출한다.
type FetchStats struct {
	Attempted      int
	FailedQueries  []string
	TruncatedPairs map[string]int
}

// CorrelateWithStats 는 Correlate 와 동일 산출에 fetch 통계를 더해 돌려준다 (#405).
func (c *Correlator) CorrelateWithStats(ctx context.Context, endTime time.Time) ([]CorrelationResult, FetchStats, error) {
	end := endTime
	start := end.Add(-c.config.Window)

	// 활성 layer (pod-level + cross-node #84 + service-impact #148) 의 입력 query 를 dedup 합집합으로
	// 모은다. CrossNodeEnabled / ServiceImpactEnabled 가 false 면 해당 layer 의 query 가 fetch 셋에서
	// 빠져 매 cycle Prometheus 부하를 추가하지 않는다. node 압박 score 처럼 cross-node 와 service-impact
	// 가 공유하는 query 는 PlannedQueries 가 한 번만 남겨 중복 fetch 를 회피한다.
	queries := c.config.PlannedQueries()

	// 각 query 를 goroutine 으로 병렬 fetch 한다. 표준 라이브러리 sync.WaitGroup + 인덱스 기반
	// 사전 할당 슬라이스로 query 순서를 보존하고 비결정성을 차단한다. 모든 query 가 독립적이고
	// fetcher 의 http.Client 가 concurrent 안전하므로 추가 동기화는 불필요하다.
	type fetchResult struct {
		series []LabeledSeries
		err    error
	}
	fetched := make([]fetchResult, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			series, err := c.fetcher.Fetch(ctx, q, start, end, c.config.Step)
			fetched[i] = fetchResult{series: series, err: err}
		}(i, q)
	}
	wg.Wait()

	all := make([]LabeledSeries, 0)
	stats := FetchStats{Attempted: len(queries)}
	var fetchErrors []string
	for i, r := range fetched {
		if r.err != nil {
			fetchErrors = append(fetchErrors, fmt.Sprintf("query %q: %v", queries[i], r.err))
			stats.FailedQueries = append(stats.FailedQueries, queries[i])
			// #405 부분 실패 가시화. 종전에는 전량 실패가 아니면 개별 실패가 로그조차 없었다.
			log.Printf("correlate: fetch failed for query %q: %v", queries[i], r.err)
			continue
		}
		all = append(all, r.series...)
	}
	// 모든 query 가 실패했을 때만 error 로 격상한다. 일부 성공 + 일부 실패 (예: 한 query 가 syntax
	// 오류 인데 다른 query 들이 정상) 는 부분 결과로 산출을 계속한다.
	if len(queries) > 0 && len(fetchErrors) == len(queries) {
		return nil, stats, fmt.Errorf("all %d queries failed: %v", len(queries), fetchErrors)
	}

	// #245 무부하 노이즈 게이트. 아래 4개 layer (pod / cross-node / service-impact / cross-level) 가
	// 모두 all 을 입력으로 페어를 열거하므로, 여기서 한 번 거르면 전 layer 와 이를 소비하는 impact
	// graph / RCA 까지 일괄 전파된다. Pearson / Granger 이전 차단이라 무부하 시 계산 비용도 함께 준다.
	all = filterWeakSuspects(all, c.config.MinSuspectScore)

	pairs := EnumeratePairs(all)
	// 동일 시계열이 여러 페어에 반복 등장하므로 samplesToValues 변환 결과를 cache 한다. 키는 시리즈
	// Samples slice 의 첫 element pointer 와 length 의 합성이라 같은 underlying array 를 다른 길이
	// 의 슬라이스가 공유하는 (prefix / 부분 슬라이스) 케이스에서 충돌이 일어나지 않는다. 페어 수가
	// N 이면 변환은 unique 시리즈 수에 선형이라 매번 변환할 때의 O(N) 슬라이스 할당과 복사를 줄인다.
	// #406 hint 는 cache 에 실제로 담기는 최대 entry 수인 unique 시계열 수 (len(all)) 다. 종전의
	// 페어 수 x2 는 페어 수 제곱 성장을 그대로 hint 로 옮겨 대형 클러스터에서 map bucket 사전 할당
	// 만으로 수십 MB 를 잡았다.
	type cacheKey struct {
		ptr *Sample
		len int
	}
	valuesCache := make(map[cacheKey][]float64, len(all))
	getValues := func(s TimeSeries) []float64 {
		if len(s.Samples) == 0 {
			return nil
		}
		key := cacheKey{ptr: &s.Samples[0], len: len(s.Samples)}
		if v, ok := valuesCache[key]; ok {
			return v
		}
		v := samplesToValues(s.Samples)
		valuesCache[key] = v
		return v
	}

	// computePodPair 는 pod 페어 1개의 전체 산정 (Pearson + Granger + EffectSize) 이다.
	//
	// #69 Granger causality 산정. src 의 과거 값이 dst 의 현재 값을 예측하는 데 통계적으로 유의한
	// 추가 정보를 제공하는지의 F-statistic 과 p-value 를 추가 첨부한다. #353 Pearson 이 선택한 lag
	// (r.MaxAbsLag) 을 그대로 쓴다: lag_seconds (Pearson 선택 lag) 와 pvalue (Granger) 가 동일 lag 을
	// 가리켜 "suspect 가 victim 을 N 초 선행하며 그 인과가 유의하다" 가 한 lag 구조로 정합한다.
	// MaxAbsLag < 1 (contemporaneous 또는 victim 선행) 이면 granger.Test 가 빈 Result 를 돌려
	// GrangerOK=false 가 되어 인과 주장을 억제한다 (collector 가 GrangerOK 일 때만 pvalue emit).
	// 표본 부족 또는 행렬 singular 케이스도 GrangerOK=false 로 자연 skip 된다.
	//
	// #146 / #175 effect size 산정. src (suspect 압박) high / low 구간의 dst (victim) 차이를 victim
	// 신호별 native 단위 절대 영향 크기로, 그 차이의 Welch t-test 유의성을 함께 산출한다. #406 방향
	// 사전필터로 dst 는 항상 victim 이라 classifyVictimSignal 이 SignalNone 이 아니다. EffectSize 는
	// high / low 각 구간에 minSamples 이상을 요구하므로 Pearson 전체 표본 임계 (MinSamples) 의 1/4
	// 을 쓴다. 같은 값을 그대로 넘기면 window / step 으로 정해진 전체 표본 (예: 30m / 30s = 60) 을
	// 양분한 각 구간이 임계 미만이 되어 거의 모든 페어가 skip 된다. 최소 2 는 보장한다.
	//
	// #363 EffectSize 를 Pearson 이 선택한 lag (r.MaxAbsLag) 에서 산정한다. suspect 가 victim 을 k
	// step 선행하면 압박 구간의 victim degradation 이 k step 뒤에 나타나므로, lag 0 원계열로 high /
	// low 를 분할하면 magnitude 와 p-value 가 희석·편향된다. Granger 와 동일 lag 으로 정렬해 Pearson
	// (lag_seconds) 과 Granger (pvalue) 와 effect 세 신호가 같은 lag 구조를 가리키게 한다.
	computePodPair := func(p Pair) CorrelationResult {
		r := PearsonWithLag(p.Src, p.Dst, c.config.LagSteps, c.config.MinSamples)
		r.Pair = p.Key
		srcVals := getValues(p.Src)
		dstVals := getValues(p.Dst)
		g := granger.Test(srcVals, dstVals, r.MaxAbsLag, c.config.GrangerMinSamples)
		r.FStatistic = g.F
		r.PValue = g.PValue
		r.GrangerOK = g.OK
		impactMin := c.config.MinSamples / 4
		if impactMin < 2 {
			impactMin = 2
		}
		victimSignal := classifyVictimSignal(p.Key.DstMetric)
		alignedSrc, alignedDst := alignByLag(srcVals, dstVals, r.MaxAbsLag)
		es := EffectSize(alignedSrc, alignedDst, victimSignal, impactMin)
		r.ImpactMagnitude = es.Magnitude
		r.ImpactMagnitudeOK = es.OK
		r.ImpactPValue = es.PValue
		r.ImpactPValueOK = es.PValueOK
		// impact_seconds 는 latency 전용 legacy 로 유지한다 (#146 호환, seconds 단위 정합).
		if victimSignal == SignalLatency {
			r.Impact = es.Magnitude
			r.ImpactOK = es.OK
		}
		return r
	}

	// #406 pod 레이어 페어 캡. 타 3레이어와 동일하게 전 페어 Pearson 산정 후 |corr| 상위로 적용한다
	// (#372 규약). 캡 이하면 종전과 완전 동일한 single pass 다. 캡 초과 시에만 경량 scoring pass
	// (score 8B/페어) 로 상위 인덱스를 고른 뒤 생존 페어만 전체 산정 (Granger / EffectSize + 결과
	// 슬라이스 보유) 을 수행해, 페어 수 제곱 성장이 결과 슬라이스 메모리로 직결되던 경로를 차단한다.
	// 생존 페어의 Pearson 이 두 번 산정되는 비용은 캡 초과 대형 클러스터에서만 발생하며 Granger /
	// EffectSize 절감 대비 미미하다.
	podMaxPairs := c.config.PodMaxPairs
	if podMaxPairs <= 0 {
		podMaxPairs = 32768
	}
	var results []CorrelationResult
	if len(pairs) <= podMaxPairs {
		results = make([]CorrelationResult, 0, len(pairs))
		for _, p := range pairs {
			results = append(results, computePodPair(p))
		}
	} else {
		scores := make([]float64, len(pairs))
		for i, p := range pairs {
			r := PearsonWithLag(p.Src, p.Dst, c.config.LagSteps, c.config.MinSamples)
			scores[i] = r.MaxAbsValue
		}
		kept := capIndicesByScore(scores, podMaxPairs)
		c.recordTruncation(&stats, "pod", len(pairs)-len(kept))
		results = make([]CorrelationResult, 0, len(kept))
		for _, i := range kept {
			results = append(results, computePodPair(pairs[i]))
		}
	}

	// #84 cross-node interference layer. CrossNodeEnabled opt-in 시 node 단위 시계열 의 페어 를
	// 추가 산출 해 IsCrossNode=true 로 마킹 한 결과 를 동일 슬라이스 에 append 한다. EnumerateNodePairs
	// 가 node 라벨 만 있는 시계열 만 후보 로 인정 하므로 본 호출 은 pod-level 페어 산출 과 완전히
	// 분리 된다. #372 페어 캡 (maxPairs, Go 빌트인 cap() 과 의 이름 충돌 회피 명명) 은 전 페어의
	// Pearson 산정 후 |corr| 상위로 적용한다. 종전에는 enumerate 사전순 앞 maxPairs 개로 산정 전에
	// 잘라, 캡 초과 시 강상관 페어가 사전순 뒤라는 이유로 top-N 에서 누락됐다. Granger 는 캡 통과
	// 페어에만 수행해 상대적으로 비싼 산정의 비용 통제 역할을 유지한다.
	if c.config.CrossNodeEnabled {
		nodePairs := EnumerateNodePairs(all)
		maxPairs := c.config.CrossNodeMaxPairs
		if maxPairs <= 0 {
			maxPairs = 1024
		}
		layer := make([]CorrelationResult, len(nodePairs))
		for i, p := range nodePairs {
			r := PearsonWithLag(p.Src, p.Dst, c.config.LagSteps, c.config.MinSamples)
			r.NodePair = p.Key
			r.IsCrossNode = true
			layer[i] = r
		}
		kept := capIndicesByScore(layerScores(layer), maxPairs)
		c.recordTruncation(&stats, "cross_node", len(layer)-len(kept))
		for _, i := range kept {
			r := layer[i]
			p := nodePairs[i]
			g := granger.Test(getValues(p.Src), getValues(p.Dst), r.MaxAbsLag, c.config.GrangerMinSamples)
			r.FStatistic = g.F
			r.PValue = g.PValue
			r.GrangerOK = g.OK
			results = append(results, r)
		}
	}

	// #148 service-impact layer. ServiceImpactEnabled opt-in 시 suspect node 압박과 victim workload
	// (Service 근사) latency 페어를 추가 산출해 IsServiceImpact=true 로 마킹한 결과를 동일 슬라이스에
	// append 한다. EnumerateServiceImpactPairs 가 node-level suspect 와 workload-level victim 만 후보로
	// 인정하므로 pod-level / cross-node 페어 산출과 완전히 분리된다.
	if c.config.ServiceImpactEnabled {
		servicePairs := EnumerateServiceImpactPairs(all)
		maxPairs := c.config.ServiceImpactMaxPairs
		if maxPairs <= 0 {
			maxPairs = 4096
		}
		// #372 캡은 Pearson 산정 후 |corr| 상위 적용 (cross-node 와 동일 규약).
		layer := make([]CorrelationResult, len(servicePairs))
		for i, p := range servicePairs {
			r := PearsonWithLag(p.Src, p.Dst, c.config.LagSteps, c.config.MinSamples)
			r.ServiceImpactPair = p.Key
			r.IsServiceImpact = true
			layer[i] = r
		}
		kept := capIndicesByScore(layerScores(layer), maxPairs)
		c.recordTruncation(&stats, "service_impact", len(layer)-len(kept))
		for _, i := range kept {
			r := layer[i]
			p := servicePairs[i]
			g := granger.Test(getValues(p.Src), getValues(p.Dst), r.MaxAbsLag, c.config.GrangerMinSamples)
			r.FStatistic = g.F
			r.PValue = g.PValue
			r.GrangerOK = g.OK
			results = append(results, r)
		}
	}

	// #149 cross-level layer. CrossLevelEnabled opt-in 시 동일 node 안에서 node 압박과 pod latency 를
	// 잇는 양방향 페어를 추가 산출해 IsCrossLevel=true 로 마킹한 결과를 동일 슬라이스에 append 한다.
	// EnumerateCrossLevelPairs 가 node-level 과 pod-level 시계열을 동일 node 로만 매칭하므로 기존 세
	// layer 의 페어 산출과 분리되며, allow-list 와 max-pairs 캡으로 카디널리티를 통제한다.
	if c.config.CrossLevelEnabled {
		crossLevelPairs := EnumerateCrossLevelPairs(all, c.config.CrossLevelAllowNamespaces)
		maxPairs := c.config.CrossLevelMaxPairs
		if maxPairs <= 0 {
			maxPairs = 4096
		}
		// #372 캡은 Pearson 산정 후 |corr| 상위 적용 (cross-node 와 동일 규약).
		layer := make([]CorrelationResult, len(crossLevelPairs))
		for i, p := range crossLevelPairs {
			r := PearsonWithLag(p.Src, p.Dst, c.config.LagSteps, c.config.MinSamples)
			r.CrossLevelPair = p.Key
			r.IsCrossLevel = true
			layer[i] = r
		}
		kept := capIndicesByScore(layerScores(layer), maxPairs)
		c.recordTruncation(&stats, "cross_level", len(layer)-len(kept))
		for _, i := range kept {
			r := layer[i]
			p := crossLevelPairs[i]
			g := granger.Test(getValues(p.Src), getValues(p.Dst), r.MaxAbsLag, c.config.GrangerMinSamples)
			r.FStatistic = g.F
			r.PValue = g.PValue
			r.GrangerOK = g.OK
			results = append(results, r)
		}
	}

	return results, stats, nil
}

// capIndicesByScore 는 #372 의 score 기준 페어 캡이다. 레이어의 전 페어 Pearson 결과에서 |corr|
// (MaxAbsValue) 내림차순 상위 maxPairs 개의 인덱스를 원래 enumerate 순서로 돌려준다. 종전 사전순
// 선두 절단은 캡 초과 시 강상관 페어를 순서 때문에 탈락시켰다. 동률은 SliceStable 로 enumerate
// 순서 (사전순) 를 유지해 결정적이고, 반환 인덱스를 오름차순 복원해 emit 순서도 종전과 같은
// enumerate 순서다. 캡 이하면 전 인덱스를 그대로 돌려줘 종전과 완전 동일 동작이다.
func capIndicesByScore(scores []float64, maxPairs int) []int {
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	if len(scores) <= maxPairs {
		return idx
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return scores[idx[a]] > scores[idx[b]]
	})
	idx = idx[:maxPairs]
	sort.Ints(idx)
	return idx
}

// recordTruncation 은 레이어 캡 절단 발생을 stats 에 기록한다 (#406). dropped 가 0 이하면 캡 미발동
// 이라 기록하지 않아, TruncatedPairs 는 실제 절단이 있는 레이어만 담는다.
func (c *Correlator) recordTruncation(stats *FetchStats, layer string, dropped int) {
	if dropped <= 0 {
		return
	}
	if stats.TruncatedPairs == nil {
		stats.TruncatedPairs = make(map[string]int)
	}
	stats.TruncatedPairs[layer] += dropped
}

// layerScores 는 레이어 결과 슬라이스에서 capIndicesByScore 입력용 |corr| score 를 추출한다.
func layerScores(layer []CorrelationResult) []float64 {
	scores := make([]float64, len(layer))
	for i, r := range layer {
		scores[i] = r.MaxAbsValue
	}
	return scores
}

// filterWeakSuspects 는 #245 의 무부하 노이즈 게이트다. suspect (victim 신호가 아닌 cause score)
// 시계열의 window 내 최대 절대값이 floor 미만이면 입력에서 제거한다. 상수 시리즈는 SkippedConstant
// 로 이미 걸러지지만 근제로 변동은 통과하고, 피어슨 상관은 크기와 무관하게 파형 유사성만으로 1.0
// 에 접근하므로 절대 크기를 여기서 강제한다. victim 시계열은 latency 초나 bytes rate 같은 native
// 단위라 게이트 대상이 아니다. floor <= 0 이면 비활성이다. floor 는 env/flag 로 들어오는 운영자
// 입력이고 strconv.ParseFloat 가 "NaN" 을 무오류로 통과시키는데, NaN 은 모든 비교가 false 라
// suspect 전체가 조용히 유실되므로 비활성으로 취급한다.
func filterWeakSuspects(items []LabeledSeries, floor float64) []LabeledSeries {
	if math.IsNaN(floor) || floor <= 0 {
		return items
	}
	out := make([]LabeledSeries, 0, len(items))
	for _, it := range items {
		if isVictimMetric(it.Metric) || seriesMaxAbs(it.Series.Samples) >= floor {
			out = append(out, it)
		}
	}
	return out
}

// seriesMaxAbs 는 표본 중 최대 절대값을 반환한다. NaN 은 비교에서 자연 탈락하고, 표본이 없거나
// 전부 NaN 이면 0 이라 게이트에 걸린다.
func seriesMaxAbs(samples []Sample) float64 {
	max := 0.0
	for _, s := range samples {
		if v := math.Abs(s.Value); v > max {
			max = v
		}
	}
	return max
}

// samplesToValues 는 Sample slice 의 Value 만 추출해 Granger 입력 형태로 변환한다. Granger 산정은
// timestamp 자체를 직접 사용하지 않으며 두 시계열의 step 이 동일하게 정렬되어 있다는 fetcher 의
// 전제를 그대로 따른다.
func samplesToValues(samples []Sample) []float64 {
	out := make([]float64, len(samples))
	for i, s := range samples {
		out[i] = s.Value
	}
	return out
}
