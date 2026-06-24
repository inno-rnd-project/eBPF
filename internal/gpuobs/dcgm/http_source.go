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
// poll 사이클 (보통 5s) 안에서 끝나도록 3s로 둔다.
const DefaultFetchTimeout = 3 * time.Second

// Available은 dcgm-exporter /metrics에 HTTP GET 후 200 응답이면 true를 돌려준다. timeout이나
// connection refused나 비-200 응답이면 false를 돌려주어 dcgm-exporter 부재 환경에서 graceful
// degradation 으로 gpuobs_dcgm_available가 0 emit된다.
func (s *httpSource) Available() bool {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
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
