// Package sources 의 gpuobs.go 는 #122 의 multi-source cross-reference 의 GPU 도메인 신호
// 추출 helper 다. node 단위 GPU dominant cause weight 또는 idle cause weight 의 Prometheus
// instant query 결과를 0-1 범위 정규화 값으로 돌려준다.
package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// fetchFailures는 하류 신호 fetch의 실패 카운터다(#446). source 라벨은 신호 갈래(gpuobs 등),
// reason 라벨은 실패 분기(request/do/status/read/parse)로 폐쇄된다. gpuobs 신호가 URL 오설정이나
// rule 이름 변경으로 영구 0이 되어 confidence를 누르는 무관측 결함의 원인 표면이다. 등록은 main이
// Collectors()로 수행한다.
var fetchFailures = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "rca_source_fetch_failures_total",
		Help: "Downstream signal fetch failures per source and failure reason (request build, transport, non-200 status, body read, parse). A persistently increasing gpuobs series means the GPU signal is silently stuck at 0 and depressing confidence.",
	},
	[]string{"source", "reason"},
)

// Collectors는 본 패키지의 메트릭을 prometheus.Registerer.MustRegister에 전달할 슬라이스로
// 돌려준다 (rcametrics.Metrics.Collectors와 동일 패턴).
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{fetchFailures}
}

// gpuSignalQueryTemplate 은 node 단위 GPU 신호 강도 의 PromQL query 다. pod:gpu_idle_cause_weight:5m
// 의 max weight 를 node 별 집계 하여 GPU idle 게이팅 활성 시간대 의 최대 cause weight 를 0-1
// 범위 로 반환 한다. 단일 query 의 결과 가 빈 vector 면 0 으로 떨어져 confidence 가 자연 감쇠
// 된다. correlation_dominant_dimension 같은 fallback query 는 본 PR 외 follow-up 영역.
const gpuSignalQueryTemplate = `max by(node) (pod:gpu_idle_cause_weight:5m{node="%s"})`

// httpGpuobsSource 는 Prometheus instant query 로 GPU 신호 를 fetch 하는 production 구현 이다.
// snapshot 과 promql source 와 동일 패턴 으로 짧은 timeout 과 빈 fallback 을 사용 한다.
type httpGpuobsSource struct {
	prometheusURL string
	client        *http.Client
	timeout       time.Duration
}

// newHTTPGpuobsSource 는 Prometheus base URL 과 fetch timeout 으로 source 를 만든다.
func newHTTPGpuobsSource(prometheusURL string, timeout time.Duration) *httpGpuobsSource {
	return &httpGpuobsSource{
		prometheusURL: strings.TrimRight(prometheusURL, "/"),
		client:        &http.Client{Timeout: timeout},
		timeout:       timeout,
	}
}

// fetchGPUSignal 은 node 단위 GPU 신호 강도 를 0-1 범위 로 돌려준다. Prometheus instant query
// 가 timeout 또는 빈 결과 면 0 을 돌려주어 confidence 산출 이 graceful empty 로 진행 된다.
// 실패 분기는 promql source와 동일하게 로그를 남기고 fetchFailures 카운터로 계수한다(#446).
// 종전에는 전 분기가 무관측 0 반환이라 GPU 신호가 영구 0이 되어도 원인 표면이 없었다.
func (s *httpGpuobsSource) fetchGPUSignal(ctx context.Context, node string) float64 {
	if s.prometheusURL == "" || node == "" {
		return 0
	}

	// #419 요청 컨텍스트 파생 (snapshot.fetch 와 동일).
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	query := fmt.Sprintf(gpuSignalQueryTemplate, escapePromLabel(node))
	reqURL := s.prometheusURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		log.Printf("rca gpuobs request: %v", err)
		fetchFailures.WithLabelValues("gpuobs", "request").Inc()
		return 0
	}

	resp, err := s.client.Do(req)
	if err != nil {
		log.Printf("rca gpuobs do %s: %v", reqURL, err)
		fetchFailures.WithLabelValues("gpuobs", "do").Inc()
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Printf("rca gpuobs status %d for %s", resp.StatusCode, reqURL)
		fetchFailures.WithLabelValues("gpuobs", "status").Inc()
		return 0
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("rca gpuobs read body: %v", err)
		fetchFailures.WithLabelValues("gpuobs", "read").Inc()
		return 0
	}

	value, err := parsePromInstantScalar(body)
	if err != nil {
		log.Printf("rca gpuobs parse: %v", err)
		fetchFailures.WithLabelValues("gpuobs", "parse").Inc()
		return 0
	}
	return value
}

// escapePromLabel 은 PromQL label value 에 들어가는 backslash 와 double-quote 를 escape 한다.
// node 이름 은 일반적 으로 alphanumeric 과 dash 라 escape 가 필요 없지만 적대적 입력 으로부터
// query injection 을 차단 한다.
func escapePromLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return v
}

// parsePromInstantScalar 는 Prometheus instant query 응답 의 result[0].value[1] 을 float 으로
// 추출 한다. encoding/json 표준 라이브러리 를 사용 해 JSON 공백 변형 과 필드 순서 변경 에 강건
// 하게 동작 한다. result 가 빈 vector 면 0 을 돌려주고 응답 schema 가 비정합 이면 에러 를 돌려
// 준다.
func parsePromInstantScalar(body []byte) (float64, error) {
	var response struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, err
	}
	if len(response.Data.Result) == 0 || len(response.Data.Result[0].Value) < 2 {
		return 0, nil
	}
	valStr, ok := response.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, errors.New("malformed prometheus value type")
	}
	return strconv.ParseFloat(valStr, 64)
}
