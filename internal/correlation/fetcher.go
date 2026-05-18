package correlation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Fetcher 는 정해진 query 와 time range / step 으로 시계열을 가져오는 인터페이스다. correlator 와
// 분리해 단위 테스트에서 mock 으로 대체할 수 있게 한다.
type Fetcher interface {
	Fetch(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]LabeledSeries, error)
}

// PrometheusFetcher 는 /api/v1/query_range 를 직접 net/http 로 호출하는 Fetcher 구현이다. github.com
// /prometheus/client_golang/api 의존성을 도입하지 않아 본 패키지의 외부 의존성을 표준 라이브러리로
// 한정한다.
type PrometheusFetcher struct {
	baseURL string
	client  *http.Client
}

// NewPrometheusFetcher 는 base URL (예: http://localhost:9090) 과 timeout 으로 fetcher 를 만든다.
// baseURL 의 trailing slash 는 정규화한다.
func NewPrometheusFetcher(baseURL string, timeout time.Duration) *PrometheusFetcher {
	return &PrometheusFetcher{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

// queryRangeResponse 는 Prometheus query_range API 의 matrix 응답 schema 다.
// https://prometheus.io/docs/prometheus/latest/querying/api/#range-queries
type queryRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string             `json:"resultType"`
		Result     []matrixResultItem `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

type matrixResultItem struct {
	Metric map[string]string `json:"metric"`
	// Values 는 [[timestamp_seconds, value_string], ...] 형태로 들어온다. timestamp 는 float64,
	// value 는 string ("NaN", "+Inf", 정상 숫자) 으로 모두 가능하다.
	Values [][2]json.RawMessage `json:"values"`
}

// Fetch 는 query_range API 를 호출해 결과를 []LabeledSeries 로 파싱한다. start / end / step 은
// 호출자가 결정해 모든 fetch 호출이 동일 값을 받으면 timestamp 정렬이 자동 보장된다.
func (f *PrometheusFetcher) Fetch(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]LabeledSeries, error) {
	u, err := url.Parse(f.baseURL + "/api/v1/query_range")
	if err != nil {
		return nil, fmt.Errorf("invalid base URL %q: %w", f.baseURL, err)
	}
	q := u.Query()
	q.Set("query", query)
	q.Set("start", strconv.FormatFloat(float64(start.UnixNano())/1e9, 'f', 3, 64))
	q.Set("end", strconv.FormatFloat(float64(end.UnixNano())/1e9, 'f', 3, 64))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', 3, 64))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", u.String(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed queryRangeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode json: %w (body: %s)", err, truncate(string(body), 200))
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus error: type=%s msg=%s", parsed.ErrorType, parsed.Error)
	}
	if parsed.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("unexpected resultType %q (query_range expects matrix)", parsed.Data.ResultType)
	}

	out := make([]LabeledSeries, 0, len(parsed.Data.Result))
	for _, item := range parsed.Data.Result {
		series := TimeSeries{Labels: item.Metric, Samples: make([]Sample, 0, len(item.Values))}
		for _, pair := range item.Values {
			var ts float64
			if err := json.Unmarshal(pair[0], &ts); err != nil {
				return nil, fmt.Errorf("decode timestamp: %w", err)
			}
			// Prometheus 는 value 를 string 으로 emit 한다. "NaN", "+Inf", "-Inf" 와 일반 숫자 모두
			// strconv.ParseFloat 가 그대로 받는다.
			var rawValue string
			if err := json.Unmarshal(pair[1], &rawValue); err != nil {
				return nil, fmt.Errorf("decode value: %w", err)
			}
			v, err := strconv.ParseFloat(rawValue, 64)
			if err != nil {
				return nil, fmt.Errorf("parse value %q: %w", rawValue, err)
			}
			series.Samples = append(series.Samples, Sample{
				TimestampMs: int64(ts * 1000),
				Value:       v,
			})
		}
		out = append(out, LabeledSeries{Metric: query, Series: series})
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
