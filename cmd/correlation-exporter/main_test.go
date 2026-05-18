package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/correlation"
	"netobs/internal/correlation/exporter"
)

// TestReconcileOnce_EmitsNoisyNeighborMetrics 는 mock Prometheus 가 query_range 응답으로 latency
// 와 cpu 두 시계열을 emit 할 때 reconcileOnce 한 번이 끝나면 collector 가 score / lag 메트릭을
// 정상 노출하고 health 카운터가 올바르게 누적되는지 검증한다.
func TestReconcileOnce_EmitsNoisyNeighborMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		// 두 시계열을 강하게 양의 상관으로 emit (값이 동일하게 변동). latency 측은 victim, cpu 측은 suspect.
		// EnumeratePairs 는 두 metric query 의 응답에서 동일 node 의 두 pod 페어를 만든다. 본 mock 은
		// metric 별 단일 series 로 응답하므로 (cpu_pod, latency_pod) 단일 페어가 결과로 산출된다.
		labelsFor := func(pod string) string {
			return `"node":"n1","src_namespace":"default","src_pod":"` + pod + `","src_pod_uid":"uid-` + pod + `"`
		}
		pod := "victim"
		metricLabel := "latency"
		if !strings.Contains(query, "latency") {
			pod = "suspect"
			metricLabel = "cpu"
		}
		_ = metricLabel
		body := `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{` + labelsFor(pod) + `},"values":[`
		// 60 샘플을 같은 진폭의 짝홀 패턴으로 emit. 두 시계열이 정확히 동일 phase 라 lag=0 에서 corr ~ 1.
		for i := 0; i < 60; i++ {
			if i > 0 {
				body += ","
			}
			ts := 1700000000 + i*30
			val := "1.0"
			if i%2 == 0 {
				val = "2.0"
			}
			body += `[` + strconv.Itoa(ts) + `,"` + val + `"]`
		}
		body += `]}]}}`
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	cfg := correlation.Config{
		PrometheusURL: srv.URL,
		Window:        30 * time.Minute,
		Step:          30 * time.Second,
		MinSamples:    30,
		LagSteps:      []int{0},
		DefaultMetrics: []string{
			"pod:cpu_throttle_score:5m",
			`histogram_quantile(0.99, sum by(node, src_namespace, src_pod, src_pod_uid, le) (rate(netobs_pod_stage_latency_labeled_seconds_bucket[5m])))`,
		},
		FetchTimeout: 5 * time.Second,
	}

	fetcher, err := correlation.NewPrometheusFetcher(cfg.PrometheusURL, cfg.FetchTimeout)
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	corr := correlation.New(fetcher, cfg)

	reg := prometheus.NewRegistry()
	collector := exporter.NewCollector(cfg.Step)
	reg.MustRegister(collector)
	health := exporter.NewHealth(reg)
	var ready atomic.Bool

	reconcileOnce(context.Background(), corr, collector, health, &ready, 10,
		cfg.FetchTimeout*time.Duration(len(cfg.DefaultMetrics)+1))

	if !ready.Load() {
		t.Fatalf("ready=false after successful reconcile")
	}

	count := testutil.CollectAndCount(collector, "correlation_noisy_neighbor_score")
	if count != 1 {
		t.Fatalf("noisy_neighbor_score count=%d want 1 (victim/suspect 단일 페어)", count)
	}

	if v := testutil.ToFloat64(health.ReconcilePairs); v == 0 {
		t.Errorf("pairs_total=0 want >0 after reconcile")
	}
	if v := testutil.ToFloat64(health.ReconcileNeighbors); v != 1 {
		t.Errorf("neighbors_total=%v want 1", v)
	}
	if v := testutil.ToFloat64(health.LastSuccessTimestamp); v == 0 {
		t.Errorf("last_success_timestamp=0 want >0 after reconcile")
	}
	if v := testutil.ToFloat64(health.ReconcileErrors); v != 0 {
		t.Errorf("errors_total=%v want 0 on success", v)
	}
}

// TestReconcileOnce_PrometheusErrorDoesNotMarkReady 는 Prometheus 가 모든 query 에 5xx 를 반환할 때
// reconcileOnce 가 error 로 종료되고 ready 가 false 로 유지되며 errors_total 만 증가하는지 검증한다.
func TestReconcileOnce_PrometheusErrorDoesNotMarkReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := correlation.Config{
		PrometheusURL:  srv.URL,
		Window:         30 * time.Minute,
		Step:           30 * time.Second,
		MinSamples:     30,
		LagSteps:       []int{0},
		DefaultMetrics: []string{"pod:cpu_throttle_score:5m"},
		FetchTimeout:   2 * time.Second,
	}
	fetcher, err := correlation.NewPrometheusFetcher(cfg.PrometheusURL, cfg.FetchTimeout)
	if err != nil {
		t.Fatalf("fetcher: %v", err)
	}
	corr := correlation.New(fetcher, cfg)

	reg := prometheus.NewRegistry()
	collector := exporter.NewCollector(cfg.Step)
	reg.MustRegister(collector)
	health := exporter.NewHealth(reg)
	var ready atomic.Bool

	reconcileOnce(context.Background(), corr, collector, health, &ready, 10, cfg.FetchTimeout*2)

	if ready.Load() {
		t.Errorf("ready=true want false on Prometheus error")
	}
	if v := testutil.ToFloat64(health.ReconcileErrors); v != 1 {
		t.Errorf("errors_total=%v want 1", v)
	}
	if v := testutil.ToFloat64(health.LastSuccessTimestamp); v != 0 {
		t.Errorf("last_success_timestamp=%v want 0 (error 가 timestamp 를 갱신해서는 안 됨)", v)
	}
}

