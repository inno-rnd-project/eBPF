// http_source.go는 #133의 dcgm-exporter HTTP endpoint 기반 production Source 구현이다. NVIDIA의
// dcgm-exporter /metrics 에 HTTP GET 해 가용성 (200 응답 여부) 만 확인한다. DCGM hardware counter
// (DCGM_FI_DEV_PCIE_REPLAY_COUNTER 등) 는 gpuobs가 re-export 하지 않고 Prometheus가 dcgm-exporter를
// 직접 스크랩하므로, 메트릭 parse / re-export 경로는 #156 에서 제거했다. 순수 Go HTTP client 라 CGO와
// libdcgm.so 의존 없이 동작하며 build tag 분리도 불요하다.
package dcgm

import (
	"context"
	"net/http"
	"time"
)

// httpSource는 dcgm-exporter /metrics를 fetch하는 production Source 구현이다. noopSource와 동일
// 한 Source 인터페이스를 만족하며 cmd/gpuobs-agent의 wire-up이 GPUOBS_DCGM_ENABLED=true 일 때
// 본 구현으로 교체한다.
type httpSource struct {
	endpoint string
	client   *http.Client
	timeout  time.Duration
}

// NewHTTPSource는 dcgm-exporter endpoint URL과 fetch timeout으로 production Source를 만든다.
// endpoint는 dcgm-exporter의 /metrics 전체 URL (예: http://dcgm-exporter.gpu-operator:9400/metrics)
// 이다. timeout이 0 이하면 DefaultFetchTimeout을 적용한다.
func NewHTTPSource(endpoint string, timeout time.Duration) Source {
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	return &httpSource{
		endpoint: endpoint,
		client:   &http.Client{Timeout: timeout},
		timeout:  timeout,
	}
}

// DefaultFetchTimeout은 dcgm-exporter HTTP fetch의 wall-clock 상한이다. gpuobs-agent의 collector
// poll 사이클 (보통 5s) 안에서 끝나도록 3s로 둔다. #177 의 재시도도 본 timeout 을 전체 budget 으로
// 공유해 Available 한 번의 총 소요가 본 상한을 넘지 않는다.
const DefaultFetchTimeout = 3 * time.Second

// dcgmMaxAttempts 는 #177 의 Available 시도 횟수 상한 (1 초기 시도 + 최대 2 재시도) 이다. dcgm-exporter
// 의 순간 장애 (connection refused 등 즉시 실패) 가 수십~수백 ms 안에 복구되는 케이스를 커버하면서,
// 전체 시도가 timeout budget 안에 들도록 한다. 진짜 hang / timeout 은 첫 시도가 budget 을 소진해 재시도
// 없이 기존처럼 false 를 돌려준다.
const dcgmMaxAttempts = 3

// dcgmRetryBackoff 는 재시도 간 짧은 고정 backoff 다. 순간 장애 복구 윈도우를 커버하도록 짧게 두며,
// var 로 선언해 단위 테스트가 1ms 등으로 오버라이드함으로써 실제 sleep 지연 없이 재시도 동작을 검증하게
// 한다. production 에서는 변경되지 않는다.
var dcgmRetryBackoff = 150 * time.Millisecond

// Available은 dcgm-exporter /metrics에 HTTP GET 후 200 응답이면 true를 돌려준다. #177 부터 timeout 을
// 전체 budget 으로 두고 그 안에서 최대 dcgmMaxAttempts 회 재시도해 순간 장애를 흡수한다. budget 을
// 공유하므로 Available 한 번의 총 wall-clock 은 기존 timeout 을 넘지 않아 poll 주기 정합이 유지된다.
// budget 소진이나 모든 시도 실패 시 false 를 돌려주어 dcgm-exporter 부재 환경에서 graceful degradation
// 으로 gpuobs_dcgm_available가 0 emit된다.
func (s *httpSource) Available() bool {
	deadline := time.Now().Add(s.timeout)
	for attempt := 0; attempt < dcgmMaxAttempts; attempt++ {
		if attempt > 0 {
			// 남은 budget 이 backoff 도 못 채우면 추가 재시도를 포기해 deadline 을 넘기지 않는다.
			if time.Until(deadline) <= dcgmRetryBackoff {
				break
			}
			time.Sleep(dcgmRetryBackoff)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if s.tryOnce(remaining) {
			return true
		}
	}
	return false
}

// tryOnce 는 남은 budget (timeout) 안에서 단일 HTTP GET 을 수행해 200 응답 여부를 돌려준다. ctx
// deadline 이 client.Timeout 보다 같거나 짧아 per-attempt 상한으로 동작한다.
func (s *httpSource) tryOnce(timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// Close는 httpSource의 리소스 정리 진입점이다. http.Client는 별도 종료가 불요하므로 nil을
// 돌려준다. noopSource와 동일한 계약을 유지한다.
func (*httpSource) Close() error {
	return nil
}
