package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PodRef 는 score 조회 시 victim / suspect 식별자 다. 빈 필드 는 해당 조건 을 생략 한다 (예: suspect.Pod
// 가 비면 victim 에 대한 모든 suspect 를 대상 으로 한다).
type PodRef struct {
	Namespace string
	Pod       string
}

// InterferenceScoreProvider 는 correlation 의 간섭 score snapshot 조회 추상화 다. CorrelationScoreClient
// 가 본 인터페이스 의 구현체 로 reconciler 에 주입 되며, 단위 테스트 는 fake 구현 으로 트리거 분기 를
// 검증 한다.
type InterferenceScoreProvider interface {
	// MaxScore 는 (victim, suspect, dimension) 매칭 noisy-neighbor 페어 의 최대 score 를 반환 한다.
	// suspect.Pod 가 비면 victim 에 대한 모든 suspect 중 최대, dimension 이 비면 모든 차원 중 최대 를
	// 본다. 매칭 페어 가 없으면 (0, false, nil) 로, 에러 와 "페어 없음" 을 구분 한다.
	MaxScore(ctx context.Context, victim, suspect PodRef, dimension string) (score float64, found bool, err error)
}

// CorrelationScoreClient 는 correlation exporter 의 /api/v1/noisy-neighbor endpoint 를 단일 query 해
// 매칭 페어 의 최대 score 를 산정 한다. polling 과 debounce 는 reconciler 의 state machine 이 책임 지고
// 본 client 는 단일 HTTP query 만 수행 해 reconcile worker 의 blocking 시간 을 HTTP timeout 안 으로
// 제한 한다. 본 분리 는 PromSpikeAsserter 와 동일 한 single-shot 설계 다.
type CorrelationScoreClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewCorrelationScoreClient 는 correlation exporter base URL 로 client 를 생성 한다.
func NewCorrelationScoreClient(baseURL string) *CorrelationScoreClient {
	return &CorrelationScoreClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// MaxScore 는 noisy-neighbor 를 server-side 필터 (victim / suspect / dimension) 로 조회 해 응답 항목
// 중 최대 score 를 반환 한다. 좁은 필터 라 항목 수 가 작지만 pagination 으로 누락 되지 않도록 큰 limit
// 을 지정 한다.
func (c *CorrelationScoreClient) MaxScore(ctx context.Context, victim, suspect PodRef, dimension string) (float64, bool, error) {
	q := url.Values{}
	if victim.Namespace != "" {
		q.Set("victim_namespace", victim.Namespace)
	}
	if victim.Pod != "" {
		q.Set("victim_pod", victim.Pod)
	}
	if suspect.Namespace != "" {
		q.Set("suspect_namespace", suspect.Namespace)
	}
	if suspect.Pod != "" {
		q.Set("suspect_pod", suspect.Pod)
	}
	if dimension != "" {
		q.Set("dimension", dimension)
	}
	q.Set("limit", "1000")
	u := fmt.Sprintf("%s/api/v1/noisy-neighbor?%s", c.BaseURL, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("correlation query: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("correlation query status=%d", resp.StatusCode)
	}
	var parsed struct {
		Items []struct {
			Score float64 `json:"score"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0, false, fmt.Errorf("decode correlation response: %w", err)
	}
	if len(parsed.Items) == 0 {
		return 0, false, nil
	}
	max := parsed.Items[0].Score
	for _, it := range parsed.Items[1:] {
		if it.Score > max {
			max = it.Score
		}
	}
	return max, true, nil
}
