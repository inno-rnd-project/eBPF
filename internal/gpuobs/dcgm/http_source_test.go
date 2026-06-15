package dcgm

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// dcgmExporterSample은 dcgm-exporter /metrics 응답의 부분 mock이다. HELP와 TYPE 주석과 PCIe
// replay counter와 NVLink bandwidth 메트릭을 포함한다.
const dcgmExporterSample = `# HELP DCGM_FI_DEV_PCIE_REPLAY_COUNTER Total number of PCIe retries.
# TYPE DCGM_FI_DEV_PCIE_REPLAY_COUNTER counter
DCGM_FI_DEV_PCIE_REPLAY_COUNTER{gpu="0",UUID="GPU-abc"} 42
DCGM_FI_DEV_PCIE_REPLAY_COUNTER{gpu="1",UUID="GPU-def"} 7
# HELP DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL NVLink bandwidth.
# TYPE DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL gauge
DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL{gpu="0",UUID="GPU-abc"} 1024.5
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-abc"} 65
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

// TestHTTPSource_MetricForward_Prefix는 prefix 필터가 정확히 동작하고 라벨과 값이 정상 파싱
// 되는지 검증한다. DCGM_FI_DEV_PCIE_REPLAY_COUNTER prefix로 2 series만 추출되고 NVLink와
// GPU_TEMP는 제외되어야 한다.
func TestHTTPSource_MetricForward_Prefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(dcgmExporterSample))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL, time.Second)
	samples := s.MetricForward("DCGM_FI_DEV_PCIE_REPLAY_COUNTER")
	if len(samples) != 2 {
		t.Fatalf("samples=%d want 2 (PCIe replay 2 series)", len(samples))
	}
	// gpu=0의 값이 42인지 확인
	var found bool
	for _, sm := range samples {
		if sm.Labels["gpu"] == "0" {
			found = true
			if sm.Value != 42 {
				t.Errorf("gpu=0 value=%v want 42", sm.Value)
			}
			if sm.Labels["UUID"] != "GPU-abc" {
				t.Errorf("gpu=0 UUID=%q want GPU-abc", sm.Labels["UUID"])
			}
		}
	}
	if !found {
		t.Errorf("gpu=0 series not found")
	}
}

// TestHTTPSource_MetricForward_EmptyPrefix는 빈 prefix가 모든 DCGM 메트릭을 돌려주는지 검증한다.
// mock 응답의 4 메트릭 줄 (PCIe 2 + NVLink 1 + GPU_TEMP 1) 이 모두 추출되어야 한다.
func TestHTTPSource_MetricForward_EmptyPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(dcgmExporterSample))
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL, time.Second)
	samples := s.MetricForward("")
	if len(samples) != 4 {
		t.Errorf("samples=%d want 4 (전체 메트릭 줄)", len(samples))
	}
}

// TestHTTPSource_MetricForward_Down은 endpoint 부재 시 빈 슬라이스를 돌려주는지 검증한다.
// graceful empty로 호출 측의 nil dereference를 차단한다.
func TestHTTPSource_MetricForward_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	s := NewHTTPSource(url, 500*time.Millisecond)
	if got := s.MetricForward(""); len(got) != 0 {
		t.Errorf("MetricForward()=%d samples want 0 (endpoint down)", len(got))
	}
}

// TestParsePromLine은 라벨 블록 유무 양쪽의 단일 줄 파싱을 검증한다.
func TestParsePromLine(t *testing.T) {
	name, labels, value, ok := parsePromLine(`DCGM_FI_DEV_PCIE_REPLAY_COUNTER{gpu="0",UUID="GPU-abc"} 42`)
	if !ok || name != "DCGM_FI_DEV_PCIE_REPLAY_COUNTER" || value != 42 || labels["gpu"] != "0" {
		t.Errorf("labeled parse: name=%q value=%v labels=%v ok=%v", name, value, labels, ok)
	}

	name2, _, value2, ok2 := parsePromLine(`metric_no_labels 1.5`)
	if !ok2 || name2 != "metric_no_labels" || value2 != 1.5 {
		t.Errorf("unlabeled parse: name=%q value=%v ok=%v", name2, value2, ok2)
	}

	if _, _, _, ok3 := parsePromLine(`malformed_line_only_name`); ok3 {
		t.Errorf("malformed line should return ok=false")
	}
}

// TestParsePromText_SkipsComments는 HELP와 TYPE 주석 줄이 skip되는지 검증한다.
func TestParsePromText_SkipsComments(t *testing.T) {
	samples := parsePromText(strings.NewReader(dcgmExporterSample), "")
	for _, s := range samples {
		if strings.HasPrefix(s.Name, "#") {
			t.Errorf("comment line leaked into samples: %q", s.Name)
		}
	}
}
