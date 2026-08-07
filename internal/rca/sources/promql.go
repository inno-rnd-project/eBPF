package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"netobs/internal/rca/registry"
)

// httpPromQLSource 는 Prometheus HTTP API 의 /api/v1/query 를 instant query 로 호출해 drop flow
// Top-N 결과를 추출한다. recording rule netobs_drop_burst:rate1m 을 sum by 5-tuple 로 집계한
// expr 을 한 번 호출한다.
type httpPromQLSource struct {
	baseURL string
	client  *http.Client
	timeout time.Duration
}

func newHTTPPromQLSource(baseURL string, fetchTimeout time.Duration) *httpPromQLSource {
	return &httpPromQLSource{
		baseURL: baseURL,
		client:  &http.Client{Timeout: fetchTimeout},
		timeout: fetchTimeout,
	}
}

// probe 는 readiness 용 connectivity 검사다. 가벼운 instant query (vector(1)) 로 Prometheus 연결과
// 200 응답을 확인 한다. 응답 body 는 읽지 않고 status code 만 본다.
func (p *httpPromQLSource) probe(ctx context.Context) error {
	if p == nil || p.client == nil {
		return fmt.Errorf("promql source not initialized")
	}
	q := url.Values{}
	q.Set("query", "vector(1)")
	endpoint := p.baseURL + "/api/v1/query?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// fetchTopDropFlows 는 namespace 필터를 거친 5-tuple 별 drop rate Top-N 을 돌려준다. namespace
// 가 빈 문자열이면 전체 namespace 가 대상이다. 외부 호출 실패 시 빈 슬라이스를 돌려주어
// mapping 의 fallback 경로로 진입한다.
func (p *httpPromQLSource) fetchTopDropFlows(ctx context.Context, namespace string, n int) []registry.DropFlowInfo {
	if n <= 0 {
		return nil
	}
	// #419 요청 컨텍스트 파생 (snapshot.fetch 와 동일).
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// topk + sum by 5-tuple. namespace 가 비어 있으면 src_namespace 필터를 생략한다.
	var expr string
	if namespace == "" {
		expr = fmt.Sprintf(
			`topk(%d, sum by (src_namespace, src_pod, src_ip, src_port, dst_ip, dst_port, protocol, drop_reason) (netobs_drop_burst:rate1m))`,
			n,
		)
	} else {
		expr = fmt.Sprintf(
			`topk(%d, sum by (src_namespace, src_pod, src_ip, src_port, dst_ip, dst_port, protocol, drop_reason) (netobs_drop_burst:rate1m{src_namespace=%q}))`,
			n, namespace,
		)
	}

	q := url.Values{}
	q.Set("query", expr)
	q.Set("time", strconv.FormatInt(time.Now().Unix(), 10))

	endpoint := p.baseURL + "/api/v1/query?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.Printf("promql request: %v", err)
		return nil
	}
	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("promql do %s: %v", endpoint, err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("promql status %d for %s", resp.StatusCode, endpoint)
		return nil
	}

	var body struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]interface{}    `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("promql decode: %v", err)
		return nil
	}
	if body.Status != "success" || body.Data.ResultType != "vector" {
		log.Printf("promql unexpected response status=%s type=%s", body.Status, body.Data.ResultType)
		return nil
	}

	out := make([]registry.DropFlowInfo, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		// Prometheus instant query 의 value 는 [unix_ts, "string_value"] 형태다.
		valStr, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		rate, err := strconv.ParseFloat(valStr, 64)
		if err != nil {
			continue
		}
		out = append(out, registry.DropFlowInfo{
			SrcNamespace: r.Metric["src_namespace"],
			SrcPod:       r.Metric["src_pod"],
			DstIP:        r.Metric["dst_ip"],
			DstPort:      r.Metric["dst_port"],
			Protocol:     r.Metric["protocol"],
			DropReason:   r.Metric["drop_reason"],
			RatePerSec:   rate,
		})
	}
	return out
}
