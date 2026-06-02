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

// PromSpikeAsserter 는 controller 의 SpikeAlertAsserter 구현체 다. LoadScenario 종료 후 polling
// window 동안 prometheus 의 ALERTS{alertstate="firing"} 시리즈 에서 z-score spike alert 발화 여부
// 를 확인 해 hit 한 alertname 목록 을 반환 한다. spike alert 이름 4 종 은 #89 의 actual rule 이름
// (deploy/gpuobs/base/prometheus-rule.yaml 의 resource-anomaly-spike 그룹) 과 정합 한다.
type PromSpikeAsserter struct {
	BaseURL      string
	HTTPClient   *http.Client
	PollWindow   time.Duration
	PollEvery    time.Duration
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

// DefaultSpikeAlertPattern 은 ALERTS 메트릭 alertname 라벨 정규식 매칭 패턴 이다. PromQL label
// matcher 의 alternation 형태 (|) 로 본 4 종 OR 매칭.
var DefaultSpikeAlertPattern = strings.Join(SpikeAlertNames, "|")

// NewPromSpikeAsserter 는 prometheus base URL 과 polling 파라미터 로 asserter 를 생성 한다.
func NewPromSpikeAsserter(baseURL string, pollWindow, pollEvery time.Duration) *PromSpikeAsserter {
	return &PromSpikeAsserter{
		BaseURL:      strings.TrimRight(baseURL, "/"),
		HTTPClient:   &http.Client{Timeout: 10 * time.Second},
		PollWindow:   pollWindow,
		PollEvery:    pollEvery,
		AlertPattern: DefaultSpikeAlertPattern,
	}
}

// Observe 는 sinceRunEnd 시각 이후 PollWindow 동안 PollEvery 간격 으로 prometheus 의 ALERTS 시리즈
// 를 polling 한다. firing alertname 1 개 이상 발견 시 즉시 반환. window 만료 까지 hit 가 없으면
// 빈 slice 반환 (error 가 아닌 정상 종료).
func (a *PromSpikeAsserter) Observe(ctx context.Context, sinceRunEnd time.Time) ([]string, error) {
	deadline := sinceRunEnd.Add(a.PollWindow)
	for {
		alerts, err := a.queryFiring(ctx)
		if err != nil {
			return nil, err
		}
		if len(alerts) > 0 {
			return alerts, nil
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(a.PollEvery):
		}
	}
}

// queryFiring 은 prometheus query API 를 호출 해 ALERTS{alertstate="firing",alertname=~pattern}
// 결과 의 alertname 라벨 distinct 집합 을 반환 한다.
func (a *PromSpikeAsserter) queryFiring(ctx context.Context) ([]string, error) {
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
	defer resp.Body.Close()
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
