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

// InstantSample 은 Prometheus instant query (벡터) 결과의 단일 시계열 점이다. synthesis API 가 health /
// pressure recording rule 의 현재 값을 단일 시점으로 조회할 때 쓰며, 라벨과 스칼라 값만 보유한다.
type InstantSample struct {
	Labels map[string]string
	Value  float64
}

// maxInstantResponseBytes 는 단일 instant query 응답에 허용되는 최대 바이트 수다 (#406). instant
// vector 는 시리즈당 한 점이라 정상 응답이 노드/pod 수천 시리즈여도 수백 KB 수준이고, 종전처럼
// range 용 상한 (maxFetchResponseBytes, 100MiB) 을 재사용하면 이상 응답 시 용도 대비 과대한 메모리
// 할당을 허용한다. 8MiB 는 정상 대비 열 배 이상의 안전 마진이다.
const maxInstantResponseBytes = 8 << 20

// sharedTransport 는 #411 의 공용 HTTP Transport 다. 기본 Transport 는 호스트당 idle 연결이 2 개라
// 핸들러가 10~17 개 쿼리를 병렬 팬아웃할 때마다 대부분이 새 커넥션을 수립하고 (TCP handshake 비용)
// 요청 종료 후 버려졌다. Prometheus 는 단일 호스트라 idle 풀을 팬아웃 폭 이상으로 올리면 커넥션이
// 재사용되어 지연과 CPU 가 함께 줄어든다. instant querier 와 range fetcher 가 같은 Transport 를
// 공유해 풀도 함께 쓴다.
var sharedTransport http.RoundTripper = &http.Transport{
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     90 * time.Second,
}

// InstantQuerier 는 Prometheus instant query (/api/v1/query) 를 수행하는 인터페이스다. range 시계열을
// 다루는 Fetcher 와 분리해, synthesis API 가 recording rule 의 현재 스칼라 값만 가볍게 조회하고 테스트
// 에서 mock 으로 대체할 수 있게 한다.
type InstantQuerier interface {
	Query(ctx context.Context, query string) ([]InstantSample, error)
}

// PrometheusInstantQuerier 는 /api/v1/query 를 net/http 로 직접 호출하는 InstantQuerier 구현이다.
// PrometheusFetcher 와 동일하게 client_golang/api 의존성 없이 표준 라이브러리만 쓴다.
type PrometheusInstantQuerier struct {
	queryURL *url.URL
	client   *http.Client
}

// NewPrometheusInstantQuerier 는 base URL (예: http://localhost:9090) 과 timeout 으로 querier 를 만든다.
// baseURL + /api/v1/query 를 startup 시점에 1회 파싱한다.
func NewPrometheusInstantQuerier(baseURL string, timeout time.Duration) (*PrometheusInstantQuerier, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/v1/query")
	if err != nil {
		return nil, fmt.Errorf("invalid prometheus base URL %q: %w", baseURL, err)
	}
	return &PrometheusInstantQuerier{
		queryURL: u,
		client:   &http.Client{Timeout: timeout, Transport: sharedTransport},
	}, nil
}

// vectorResponse 는 Prometheus instant query API 의 vector 응답 schema 다.
// https://prometheus.io/docs/prometheus/latest/querying/api/#instant-queries
type vectorResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string  `json:"metric"`
			Value  [2]json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Query 는 instant query API 를 호출해 결과를 []InstantSample 로 파싱한다. 결과가 비면 빈 슬라이스를
// 돌려주어 (에러 아님) synthesis API 가 데이터 부재를 graceful 하게 처리하게 한다. scalar / matrix 등
// vector 가 아닌 resultType 은 에러로 본다.
func (q *PrometheusInstantQuerier) Query(ctx context.Context, query string) ([]InstantSample, error) {
	u := *q.queryURL
	v := url.Values{}
	v.Set("query", query)
	// #235 요청 스코프 평가 시점. WithQueryTime 으로 심긴 시점이 있으면 Prometheus instant query 의
	// time 파라미터로 전달해 과거 시점 평가 (사건 시점 재구성) 를 수행한다. 미지정 시 기존과 동일하게
	// 현재 시점 평가다.
	if t, ok := QueryTimeFrom(ctx); ok {
		v.Set("time", strconv.FormatInt(t.Unix(), 10))
	}
	u.RawQuery = v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", u.String(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInstantResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > maxInstantResponseBytes {
		return nil, fmt.Errorf("response body exceeded %d bytes (limit reached)", maxInstantResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var parsed vectorResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode json: %w (body: %s)", err, truncate(string(body), 200))
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus error: type=%s msg=%s", parsed.ErrorType, parsed.Error)
	}
	if parsed.Data.ResultType != "vector" {
		return nil, fmt.Errorf("unexpected resultType %q (instant query expects vector)", parsed.Data.ResultType)
	}

	out := make([]InstantSample, 0, len(parsed.Data.Result))
	for _, item := range parsed.Data.Result {
		// instant query 의 value 는 [unix_ts, "value_string"] 형태다. value 만 채택한다.
		var rawValue string
		if err := json.Unmarshal(item.Value[1], &rawValue); err != nil {
			return nil, fmt.Errorf("decode value: %w", err)
		}
		val, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			return nil, fmt.Errorf("parse value %q: %w", rawValue, err)
		}
		out = append(out, InstantSample{Labels: item.Metric, Value: val})
	}
	return out, nil
}
