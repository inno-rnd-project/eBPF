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

// PromSpikeAsserter 는 controller 의 SpikeAlertAsserter 구현체 다. prometheus 의 ALERTS 시리즈 를
// 1 회 query 해 firing 상태 의 spike alert (alertname 4 종 OR 매칭) 의 distinct 셋 을 반환 한다.
// polling loop 와 timeout 관리 는 reconciler 의 state machine 이 책임 지고 본 asserter 는 단일
// query 만 수행 한다. 본 분리 로 reconcile worker 의 blocking 시간 이 단일 HTTP query 의 timeout
// 안 으로 제한 된다.
type PromSpikeAsserter struct {
	BaseURL      string
	HTTPClient   *http.Client
	AlertPattern string
}

// SpikeAlertNames 는 #89 가 emit 하는 spike alert 이름 enum 이다. controller 가 LoadScenario 종료
// 직후 본 4 종 중 1 개 이상 firing 이면 spec.spikeAlertAssertion = true 의 수용 조건 으로 인정 한다.
var SpikeAlertNames = []string{
	"GPUUtilSpikeDetected",
	"NetworkDropSpikeDetected",
	"CPUThrottleSpikeDetected",
	"MemoryPressureSpikeDetected",
}

// DefaultSpikeAlertPattern 은 ALERTS 메트릭 alertname 라벨 정규식 매칭 패턴 이다.
var DefaultSpikeAlertPattern = strings.Join(SpikeAlertNames, "|")

// NewPromSpikeAsserter 는 prometheus base URL 로 asserter 를 생성 한다.
func NewPromSpikeAsserter(baseURL string) *PromSpikeAsserter {
	return &PromSpikeAsserter{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		HTTPClient:   &http.Client{Timeout: 10 * time.Second},
		AlertPattern: DefaultSpikeAlertPattern,
	}
}

// Observe 는 SpikeAlertAsserter 인터페이스 의 single-shot 구현체 다. sinceRunEnd 인자 는 인터페이스
// 호환 을 위해 받지만 본 구현 에서 사용 되지 않는다 (polling window 관리 는 reconciler 가 담당).
func (a *PromSpikeAsserter) Observe(ctx context.Context, sinceRunEnd time.Time) ([]string, error) {
	q := fmt.Sprintf(`ALERTS{alertstate="firing",alertname=~"%s"}`, a.AlertPattern)
	u := fmt.Sprintf("%s/api/v1/query?query=%s", a.BaseURL, url.QueryEscape(q))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus query status=%d", resp.StatusCode)
	}
	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus status=%q", parsed.Status)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(parsed.Data.Result))
	for _, r := range parsed.Data.Result {
		name := r.Metric["alertname"]
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}
