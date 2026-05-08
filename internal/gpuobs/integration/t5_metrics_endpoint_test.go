//go:build integration

package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"netobs/internal/gpuobs/metrics"
	"netobs/internal/server"
)

// TestT5_MetricsEndpoint 는 server.NewHandler 가 발행하는 /metrics 응답이 Prometheus text format
// 을 깨뜨리지 않으며, 본 PR 의 새 메트릭 (gpuobs_cuda_pid_multi_gpu_count) 의 라벨 카디널리티가
// 의도된 범위 (노드당 1 시리즈) 안에 머무는지 검증한다.
//
// /metrics 응답을 expfmt parser 로 디코드해 metric family 단위로 검증하므로, 서식 drift / 메트릭
// 등록 누락 / 라벨 카디널리티 회귀가 통합 레이어에서 즉시 잡힌다.
func TestT5_MetricsEndpoint(t *testing.T) {
	metrics.ResetCudaStateForTest()

	reg := prometheus.NewRegistry()
	metrics.Register(reg)

	// 새 메트릭 라벨 카디널리티 검증을 위해 의도된 시리즈를 사전 발행한다. 같은 노드의 반복 호출은
	// idempotent Set 으로 시리즈 수가 늘어나지 않아야 한다.
	metrics.SetCudaPidMultiGPUCount("node-A", 0)
	metrics.SetCudaPidMultiGPUCount("node-A", 3)
	metrics.SetCudaPidMultiGPUCount("node-A", 0)

	// symbol_available / events_lost_total 은 첫 Set / Add 시점에야 시리즈가 만들어지므로 noop 호출로
	// 시리즈 등장만 트리거한다 (production 의 cuda Reader Run 이 startup 시점에 동일 호출을 한다).
	metrics.SetCudaSymbolAvailability("node-A", "cuLaunchKernel", false)
	metrics.AddCudaEventsLost("node-A", 0)

	handler := server.NewHandler("gpuobs-agent", reg, func() (bool, string) { return true, "" })
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status=%d want 200", resp.StatusCode)
	}

	parser := &expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatalf("text format parse: %v (body invalid Prometheus format)", err)
	}

	// 본 PR 가 등록한 메트릭 family 가 모두 노출되는지 확인한다.
	mustHave := []string{
		"gpuobs_cuda_pid_multi_gpu_count",
		"gpuobs_cuda_symbol_available",
		"gpuobs_cuda_events_lost_total",
	}
	for _, name := range mustHave {
		if _, ok := families[name]; !ok {
			t.Errorf("metric family %q missing from /metrics output", name)
		}
	}

	// gpuobs_cuda_pid_multi_gpu_count 는 노드당 1 시리즈이어야 한다 (idempotent Set 검증).
	if fam := families["gpuobs_cuda_pid_multi_gpu_count"]; fam != nil {
		if got := len(fam.Metric); got != 1 {
			t.Errorf("gpuobs_cuda_pid_multi_gpu_count series count=%d want 1 (single node, repeated set)", got)
		} else {
			labels := fam.Metric[0].Label
			gotNode := ""
			for _, l := range labels {
				if l.GetName() == "node" {
					gotNode = l.GetValue()
				}
			}
			if gotNode != "node-A" {
				t.Errorf("series node label=%q want node-A", gotNode)
			}
			if got := fam.Metric[0].Gauge.GetValue(); got != 0 {
				t.Errorf("gauge value=%v want 0 (last set)", got)
			}
		}
	}
}

// TestT5_ReadyzEndpoint 는 /readyz 가 ready func 의 결과를 그대로 반영하는지 검증한다. 본 PR 의 cuda
// uprobe ready 신호와 collector ready 신호가 main.go 의 ready 함수로 합산되는데, 본 통합 테스트는
// 그 합산 결과가 HTTP 응답으로 정확히 노출되는지를 cover 한다.
func TestT5_ReadyzEndpoint(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.Register(reg)

	notReady := func() (bool, string) { return false, "test reason" }
	srvNotReady := httptest.NewServer(server.NewHandler("gpuobs-agent", reg, notReady))
	defer srvNotReady.Close()

	resp, err := http.Get(srvNotReady.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("not-ready /readyz status=%d want 503", resp.StatusCode)
	}

	ready := func() (bool, string) { return true, "" }
	srvReady := httptest.NewServer(server.NewHandler("gpuobs-agent", reg, ready))
	defer srvReady.Close()

	resp2, err := http.Get(srvReady.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz (ready): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("ready /readyz status=%d want 200", resp2.StatusCode)
	}
}

// 보조: 응답 body 가 Prometheus format 이라는 것을 빨리 확인하기 위한 sanity check (parser 에러로
// 이미 잡히지만 명시적 가독성 보장). 호출하지 않으면 unused 라 vet 가 경고 → 사용처를 두지 않는다.
var _ = strings.HasPrefix
