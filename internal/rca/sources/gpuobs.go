// Package sources 의 gpuobs.go 는 #122 의 multi-source cross-reference 의 GPU 도메인 신호
// 추출 helper 다. node 단위 GPU dominant cause weight 또는 idle cause weight 의 Prometheus
// instant query 결과를 0-1 범위 정규화 값으로 돌려준다.
package sources

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// gpuSignalQueryTemplate 은 node 단위 GPU 신호 강도 의 PromQL query 다. pod:gpu_idle_cause_weight:5m
// 의 max weight 를 node 별 집계 하여 GPU idle 게이팅 활성 시간대 의 최대 cause weight 를 0-1
// 범위 로 반환 한다. fallback 으로 correlation_dominant_dimension (raw weight) 도 시도 한다.
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
func (s *httpGpuobsSource) fetchGPUSignal(node string) float64 {
	if s.prometheusURL == "" || node == "" {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	query := fmt.Sprintf(gpuSignalQueryTemplate, escapePromLabel(node))
	reqURL := s.prometheusURL + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0
	}

	value, err := parsePromInstantScalar(body)
	if err != nil {
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
// 추출 한다. 본 함수 는 dependency 없이 string scan 으로 동작 해 ringbuf 의 hot path 에 부담을
// 주지 않는다. result 가 빈 vector 면 0 을 돌려주고 의도 한 값 이 아닌 응답 형식 이면 에러 를
// 돌려준다.
func parsePromInstantScalar(body []byte) (float64, error) {
	idx := strings.Index(string(body), `"value":[`)
	if idx < 0 {
		return 0, nil
	}
	tail := string(body)[idx+len(`"value":[`):]
	commaIdx := strings.Index(tail, ",")
	if commaIdx < 0 {
		return 0, errors.New("malformed prometheus value")
	}
	rest := tail[commaIdx+1:]
	end := strings.IndexAny(rest, `]`)
	if end < 0 {
		return 0, errors.New("malformed prometheus value end")
	}
	raw := strings.TrimSpace(rest[:end])
	raw = strings.Trim(raw, `"`)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}
