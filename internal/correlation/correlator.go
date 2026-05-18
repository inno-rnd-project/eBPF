package correlation

import (
	"context"
	"fmt"
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

// Correlate 는 다음 순서로 산출을 수행한다.
//
//  1. config 의 DefaultMetrics 와 ExtraMetrics 를 합쳐 모든 query 를 동일 start / end / step 으로
//     fetch
//  2. 모든 LabeledSeries 를 EnumeratePairs 로 노드 한정 페어로 변환
//  3. 각 페어를 PearsonWithLag 로 lag 별 산출
//  4. 산출 결과를 []CorrelationResult 로 반환
//
// 일부 query 가 fetch 실패해도 나머지로 산출을 계속한다. 모든 query 가 실패할 때만 wrapped error 를
// 반환한다. fetch 결과가 비어 있는 (시계열 0개) query 는 정상으로 보고 페어 산출에서 자연 제외된다.
func (c *Correlator) Correlate(ctx context.Context) ([]CorrelationResult, error) {
	end := time.Now()
	start := end.Add(-c.config.Window)

	queries := append([]string{}, c.config.DefaultMetrics...)
	queries = append(queries, c.config.ExtraMetrics...)

	all := make([]LabeledSeries, 0)
	var fetchErrors []string
	for _, q := range queries {
		series, err := c.fetcher.Fetch(ctx, q, start, end, c.config.Step)
		if err != nil {
			fetchErrors = append(fetchErrors, fmt.Sprintf("query %q: %v", q, err))
			continue
		}
		all = append(all, series...)
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
