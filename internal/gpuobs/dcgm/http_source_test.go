package dcgm

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// dcgmExporterSample은 dcgm-exporter /metrics 응답의 부분 mock이다. Available 테스트가 200 응답
// body로 사용한다. gpuobs는 본 메트릭을 parse하지 않고 (re-export 제거, #156) Prometheus가
// dcgm-exporter를 직접 스크랩하므로, 본 상수는 200 응답 형태의 예시일 뿐이다.
const dcgmExporterSample = `# HELP DCGM_FI_DEV_PCIE_REPLAY_COUNTER Total number of PCIe retries.
# TYPE DCGM_FI_DEV_PCIE_REPLAY_COUNTER counter
DCGM_FI_DEV_PCIE_REPLAY_COUNTER{gpu="0",UUID="GPU-abc"} 42
`

// TestHTTPSource_Available_OK는 dcgm-exporter가 200을 돌려줄 때 Available이 true를 돌려주는지
// 검증한다. 데이터센터 GPU 환경에서 gpuobs_dcgm_available=1 emit의 회귀 가드다.
func TestHTTPSource_Available_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(dcgmExporterSample))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL, time.Second)
	if !s.Available() {
		t.Errorf("Available()=false want true (200 응답)")
	}
}

// TestHTTPSource_Available_Down은 dcgm-exporter가 부재 (connection refused) 일 때 Available이
// false를 돌려주는지 검증한다. dev cluster RTX 3090 환경의 graceful degradation 회귀 가드다.
func TestHTTPSource_Available_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // 즉시 닫아 connection refused 유발

	s := NewHTTPSource(url, 500*time.Millisecond)
	if s.Available() {
		t.Errorf("Available()=true want false (endpoint down)")
	}
}

// TestHTTPSource_Available_Non200은 비-200 응답 시 Available이 false인지 검증한다.
func TestHTTPSource_Available_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL, time.Second)
	if s.Available() {
		t.Errorf("Available()=true want false (503 응답)")
	}
}
