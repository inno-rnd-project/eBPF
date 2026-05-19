package exporter

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/injector/blastradius"
	"netobs/internal/injector/loadgen"
)

func sampleResult() BlastResult {
	return BlastResult{
		TargetNamespace: "default",
		TargetPod:       "victim",
		TargetNode:      "gpu",
		Kind:            loadgen.KindCPU,
		Victim: blastradius.VictimCandidate{
			Namespace: "default",
			Pod:       "neighbor",
			PodUID:    "uid-n",
			Node:      "gpu",
		},
		Score:    0.45,
		Status:   blastradius.StatusOK,
		Baseline: 0.001,
		Impact:   0.00145,
	}
}

// TestCollector_EmitsActiveAndBlast 는 SetActive 와 ReplaceBlast 후 4 종 메트릭이 정상 emit 되는
// 지 검증한다.
func TestCollector_EmitsActiveAndBlast(t *testing.T) {
	c := NewCollector()
	c.SetActive("default", "victim", "gpu", loadgen.KindCPU, 1)
	c.ReplaceBlast([]BlastResult{sampleResult()})

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	expected := `
# HELP correlation_blast_radius_score 부하 윈도우 동안 victim latency 가 baseline 대비 얼마나 증가했는지를 0 ~ 1 정규화한 score. status=ok 인 결과만 emit 된다.
# TYPE correlation_blast_radius_score gauge
correlation_blast_radius_score{kind="cpu",target_namespace="default",target_node="gpu",target_pod="victim",victim_namespace="default",victim_pod="neighbor",victim_pod_uid="uid-n"} 0.45
# HELP injector_active workload-injector 의 부하 활성 상태. 1 (활성) / 0 (비활성). 부하 종료 직후 reset 후 linger 동안 0 으로 유지되어 마지막 scrape 가 transition 을 정확히 잡도록 한다.
# TYPE injector_active gauge
injector_active{kind="cpu",target_namespace="default",target_node="gpu",target_pod="victim"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"injector_active", "correlation_blast_radius_score"); err != nil {
		t.Errorf("metric mismatch: %v", err)
	}
}

// TestCollector_ClearActiveRemovesSeries 는 ClearActive 후 active 시계열이 0 series 로 emit 되는지
// 검증한다 (Job 종료 직전 호출되어 운영자가 cleanup 을 visually 확인 가능).
func TestCollector_ClearActiveRemovesSeries(t *testing.T) {
	c := NewCollector()
	c.SetActive("default", "victim", "gpu", loadgen.KindCPU, 1)
	if testutil.CollectAndCount(c, "injector_active") != 1 {
		t.Fatal("initial active count != 1")
	}
	c.ClearActive()
	if got := testutil.CollectAndCount(c, "injector_active"); got != 0 {
		t.Errorf("after ClearActive count=%d want 0", got)
	}
}

// TestCollector_ReplaceClearsStaleBlast 는 ReplaceBlast 가 직전 snapshot 의 victim 라벨을 stale
// 없이 교체하는지 검증한다.
func TestCollector_ReplaceClearsStaleBlast(t *testing.T) {
	c := NewCollector()
	r1 := sampleResult()
	r1.Victim.Pod = "stale-victim"
	c.ReplaceBlast([]BlastResult{r1})
	if testutil.CollectAndCount(c, "correlation_blast_radius_score") != 1 {
		t.Fatal("initial blast count != 1")
	}

	r2 := sampleResult()
	r2.Victim.Pod = "fresh-victim"
	c.ReplaceBlast([]BlastResult{r2})

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "correlation_blast_radius_score" {
			continue
		}
		for _, m := range mf.Metric {
			for _, lp := range m.Label {
				if lp.GetName() == "victim_pod" && lp.GetValue() == "stale-victim" {
					t.Errorf("stale victim survived replace")
				}
			}
		}
	}
}

// TestCollector_NonOKStatusSkipped 는 status != ok 결과가 메트릭에서 제외되는지 검증한다.
func TestCollector_NonOKStatusSkipped(t *testing.T) {
	c := NewCollector()
	r := sampleResult()
	r.Status = blastradius.StatusSkippedLowBaseline
	c.ReplaceBlast([]BlastResult{r})
	if got := testutil.CollectAndCount(c, "correlation_blast_radius_score"); got != 0 {
		t.Errorf("non-ok count=%d want 0", got)
	}
}

// TestCollector_ConcurrentReplaceAndCollect 는 cycle 갱신과 scrape 가 동시 호출되어도 race 가 없는
// 지 race detector 로 검증한다.
func TestCollector_ConcurrentReplaceAndCollect(t *testing.T) {
	c := NewCollector()
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				r := sampleResult()
				r.Score = float64(i%100) / 100.0
				c.ReplaceBlast([]BlastResult{r})
				c.SetActive("default", "victim", "gpu", loadgen.KindCPU, 1)
				i++
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = reg.Gather()
		}
	}()
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()
}

// TestHealth_RecordRunAccumulates 는 RecordRun 호출이 누적 카운터를 갱신하는지 검증한다.
func TestHealth_RecordRunAccumulates(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHealth(reg)
	h.RecordRun(loadgen.KindCPU, "ok")
	h.RecordRun(loadgen.KindCPU, "ok")
	h.RecordRun(loadgen.KindNetwork, "error")
	if v := testutil.ToFloat64(h.InjectorRuns.WithLabelValues("cpu", "ok")); v != 2 {
		t.Errorf("cpu ok=%v want 2", v)
	}
	if v := testutil.ToFloat64(h.InjectorRuns.WithLabelValues("network", "error")); v != 1 {
		t.Errorf("network error=%v want 1", v)
	}
}

// TestHealth_RecordDuration 은 RecordDuration 이 초 단위 walltime 을 정확히 기록하는지 검증한다.
func TestHealth_RecordDuration(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHealth(reg)
	h.RecordDuration(loadgen.KindCPU, 300*time.Second)
	if v := testutil.ToFloat64(h.InjectorDuration.WithLabelValues("cpu")); v != 300 {
		t.Errorf("duration=%v want 300", v)
	}
}
