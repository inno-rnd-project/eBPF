package correlation

import (
	"context"
	"fmt"
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
func (c *Correlator) Correlate(ctx context.Context, endTime time.Time) ([]CorrelationResult, error) {
	end := endTime
	start := end.Add(-c.config.Window)

	queries := append([]string{}, c.config.DefaultMetrics...)
	queries = append(queries, c.config.ExtraMetrics...)

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
	var fetchErrors []string
	for i, r := range fetched {
		if r.err != nil {
			fetchErrors = append(fetchErrors, fmt.Sprintf("query %q: %v", queries[i], r.err))
			continue
		}
		all = append(all, r.series...)
	}
	// 모든 query 가 실패했을 때만 error 로 격상한다. 일부 성공 + 일부 실패 (예: 한 query 가 syntax
	// 오류 인데 다른 query 들이 정상) 는 부분 결과로 산출을 계속한다.
	if len(queries) > 0 && len(fetchErrors) == len(queries) {
		return nil, fmt.Errorf("all %d queries failed: %v", len(queries), fetchErrors)
	}

	pairs := EnumeratePairs(all)
	// 동일 시계열이 여러 페어에 반복 등장하므로 samplesToValues 변환 결과를 cache 한다. 키는 시리즈
	// Samples slice 의 첫 element pointer 와 length 의 합성이라 같은 underlying array 를 다른 길이
	// 의 슬라이스가 공유하는 (prefix / 부분 슬라이스) 케이스에서 충돌이 일어나지 않는다. 페어 수가
	// N 이면 변환은 unique 시리즈 수에 선형이라 매번 변환할 때의 O(N) 슬라이스 할당과 복사를 줄인다.
	type cacheKey struct {
		ptr *Sample
		len int
	}
	valuesCache := make(map[cacheKey][]float64, len(pairs)*2)
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

	results := make([]CorrelationResult, 0, len(pairs))
	for _, p := range pairs {
		r := PearsonWithLag(p.Src, p.Dst, c.config.LagSteps, c.config.MinSamples)
		r.Pair = p.Key
		// #69 Granger causality 산정. src 의 과거 값이 dst 의 현재 값을 예측하는 데 통계적으로 유의한
		// 추가 정보를 제공하는지의 F-statistic 과 p-value 를 추가 첨부한다. 표본 부족 또는 행렬
		// singular 케이스는 GrangerOK=false 로 자연 skip 된다.
		srcVals := getValues(p.Src)
		dstVals := getValues(p.Dst)
		g := granger.Test(srcVals, dstVals, c.config.GrangerLag, c.config.GrangerMinSamples)
		r.FStatistic = g.F
		r.PValue = g.PValue
		r.GrangerOK = g.OK
		results = append(results, r)
	}
	return results, nil
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
