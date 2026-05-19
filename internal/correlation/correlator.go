package correlation

import (
	"context"
	"fmt"
	"sync"
	"time"
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
	results := make([]CorrelationResult, 0, len(pairs))
	for _, p := range pairs {
		r := PearsonWithLag(p.Src, p.Dst, c.config.LagSteps, c.config.MinSamples)
		r.Pair = p.Key
		results = append(results, r)
	}
	return results, nil
}
