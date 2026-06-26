package dcgm

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// dcgmExporterSample은 Available 테스트의 200 응답 body다. Available은 status code (200) 만 확인하고
// body는 읽지 않으므로 (re-export 제거, #156) 내용은 무의미해 빈 문자열로 둔다.
const dcgmExporterSample = ""

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

// TestHTTPSource_Available_RecoversAfterTransientFailure 는 #177 의 수용 조건이다. dcgm-exporter 가
// 첫 응답만 일시 실패 (503) 하고 이후 복구 (200) 하면 timeout budget 내 재시도로 Available 이 true 를
// 돌려주는지 httptest 로 입증한다. 재시도가 없던 기존 구현이면 첫 503 으로 false 가 됐을 케이스다.
func TestHTTPSource_Available_RecoversAfterTransientFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 첫 호출만 일시 실패
			return
		}
		_, _ = w.Write([]byte(dcgmExporterSample)) // 이후 복구
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL, time.Second)
	if !s.Available() {
		t.Errorf("Available()=false want true (1회 일시 실패 후 재시도 복구)")
	}
	if got := calls.Load(); got < 2 {
		t.Errorf("calls=%d want >=2 (재시도가 발생해야 복구)", got)
	}
}

// TestHTTPSource_Available_FalseAfterRetriesExhausted 는 #177 의 수용 조건이다. 모든 시도가 실패하면
// 재시도 상한 (dcgmMaxAttempts) 까지 시도한 뒤 graceful false 를 돌려주는지, 그리고 시도 횟수가 상한을
// 넘지 않는지 입증한다.
func TestHTTPSource_Available_FalseAfterRetriesExhausted(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable) // 항상 실패
	}))
	defer srv.Close()

	s := NewHTTPSource(srv.URL, time.Second)
	if s.Available() {
		t.Errorf("Available()=true want false (재시도 상한 초과)")
	}
	if got := calls.Load(); got != dcgmMaxAttempts {
		t.Errorf("calls=%d want %d (상한까지만 재시도)", got, dcgmMaxAttempts)
	}
}
