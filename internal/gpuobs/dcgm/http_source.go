// http_source.go는 #133의 dcgm-exporter HTTP endpoint 기반 production Source 구현이다. NVIDIA
// 의 dcgm-exporter가 노출하는 /metrics (Prometheus text format) 를 HTTP fetch해 DCGM hardware
// counter (DCGM_FI_DEV_PCIE_REPLAY_COUNTER 와 DCGM_FI_DEV_NVLINK_BANDWIDTH_* 등) 를 Sample로
// 변환한다. 순수 Go HTTP client 라 CGO와 libdcgm.so 의존 없이 동작하며 build tag 분리도 불요하다.
// dcgm-exporter는 데이터센터 GPU (A100과 H100 등) 환경의 NVIDIA GPU Operator 표준 배포물이다.
package dcgm

import (
	"bufio"
	"context"
	"net/http"
	"strconv"
	"strings"
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

// MetricForward는 dcgm-exporter /metrics 응답의 Prometheus text format을 parse해 prefix 매칭
// 메트릭을 Sample 슬라이스로 돌려준다. prefix가 빈 문자열이면 모든 DCGM 메트릭을 돌려준다.
// fetch 실패나 parse 실패 시 빈 슬라이스를 돌려주어 호출 측이 graceful empty로 진행한다.
func (s *httpSource) MetricForward(prefix string) []Sample {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return nil
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	return parsePromText(resp.Body, prefix)
}

// Close는 httpSource의 리소스 정리 진입점이다. http.Client는 별도 종료가 불요하므로 nil을
// 돌려준다. noopSource와 동일한 계약을 유지한다.
func (*httpSource) Close() error {
	return nil
}

// parsePromText는 Prometheus text exposition format을 줄 단위로 parse해 prefix 매칭 메트릭을
// Sample 슬라이스로 변환한다. dcgm-exporter는 표준 Prometheus exposition format을 노출하므로
// 의존성 없이 줄 단위 scan으로 충분하다. HELP와 TYPE 주석 줄은 skip하고 메트릭 줄의 이름과
// 라벨과 값을 추출한다. 본 함수는 io.Reader를 받아 httptest 기반 단위 테스트가 mock 응답을
// 직접 주입 가능하게 한다.
func parsePromText(r interface{ Read([]byte) (int, error) }, prefix string) []Sample {
	out := make([]Sample, 0)
	scanner := bufio.NewScanner(r)
	// dcgm-exporter 메트릭 줄이 길 수 있어 (다수 라벨) scanner buffer를 1 MiB로 확장한다.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := parsePromLine(line)
		if !ok {
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		out = append(out, Sample{
			Name:   name,
			Labels: labels,
			Value:  value,
		})
	}
	return out
}

// parsePromLine은 Prometheus text format의 단일 메트릭 줄을 (name, labels, value) 로 분해한다.
// "DCGM_FI_DEV_PCIE_REPLAY_COUNTER{gpu=\"0\",UUID=\"GPU-xxx\"} 42" 형태를 파싱한다. 라벨 블록이
// 없는 "metric_name 1.5" 형태도 처리한다. 파싱 실패 시 ok=false를 돌려준다.
func parsePromLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	labels = map[string]string{}

	braceOpen := strings.IndexByte(line, '{')
	if braceOpen >= 0 {
		braceClose := strings.LastIndexByte(line, '}')
		if braceClose < braceOpen {
			return "", nil, 0, false
		}
		name = strings.TrimSpace(line[:braceOpen])
		labelBlock := line[braceOpen+1 : braceClose]
		parseLabelBlock(labelBlock, labels)
		rest := strings.TrimSpace(line[braceClose+1:])
		value, ok = parseLeadingFloat(rest)
		return name, labels, value, ok
	}

	// 라벨 블록 부재. "name value [timestamp]" 형태.
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil, 0, false
	}
	name = fields[0]
	value, ok = parseLeadingFloat(fields[1])
	return name, labels, value, ok
}

// parseLabelBlock은 "k1=\"v1\",k2=\"v2\"" 형태의 라벨 블록을 labels 맵에 채운다. 값의 double
// quote는 제거한다. 라벨 값에 콤마가 포함되는 케이스는 dcgm-exporter 메트릭에서 드물어 단순
// 콤마 split으로 처리한다.
func parseLabelBlock(block string, labels map[string]string) {
	for _, pair := range strings.Split(block, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		v = strings.Trim(v, `"`)
		labels[k] = v
	}
}

// parseLeadingFloat은 문자열의 선행 토큰을 float으로 파싱한다. "42 1620000000000" 같은 value +
// timestamp 형태에서 첫 토큰만 취한다.
func parseLeadingFloat(s string) (float64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
