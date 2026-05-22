package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// gaugeValue 는 PedanticRegistry 에서 단일 시리즈 gauge 의 현재 값을 끌어와 assertion 을 짧게 만든다.
// CounterFunc / Counter / Gauge 모두 mfs 의 Metric[0].GetGauge() 또는 GetCounter() 로 동일 패턴이라
// 본 헬퍼는 testutil.ToFloat64 를 사용해 메트릭 타입 분기를 collector 측에 위임한다.
func gaugeValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	return testutil.ToFloat64(c)
}

// TestSetBpfProgramLoaded 는 1/0 setter 가 라벨 별로 분리된 시리즈를 emit 하는지 검증한다.
// loader 의 pre-registration 패턴이 attach 실패 심볼을 0 으로 노출하는 회귀 가드다.
func TestSetBpfProgramLoaded(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(bpfProgramLoaded)

	SetBpfProgramLoaded("tcp_sendmsg", true)
	SetBpfProgramLoaded("veth_xmit", false)

	if got := testutil.ToFloat64(bpfProgramLoaded.WithLabelValues("tcp_sendmsg")); got != 1 {
		t.Errorf("tcp_sendmsg=%v want 1 (loaded)", got)
	}
	if got := testutil.ToFloat64(bpfProgramLoaded.WithLabelValues("veth_xmit")); got != 0 {
		t.Errorf("veth_xmit=%v want 0 (not loaded)", got)
	}
}

// TestAddBpfRingbufDrops 는 delta 누적이 단조 증가하고 0 delta 가 no-op 인지 검증한다. refresher
// 가 reset 케이스에서 0 을 전달하지 않고 호출 자체를 skip 하므로 본 함수의 0 가드는 방어용이다.
func TestAddBpfRingbufDrops(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(bpfRingbufDropsTotal)

	AddBpfRingbufDrops(0)
	if got := gaugeValue(t, bpfRingbufDropsTotal); got != 0 {
		t.Errorf("after 0 delta=%v want 0", got)
	}

	AddBpfRingbufDrops(5)
	AddBpfRingbufDrops(3)
	if got := gaugeValue(t, bpfRingbufDropsTotal); got != 8 {
		t.Errorf("after 5+3 delta=%v want 8", got)
	}
}

// TestSetBpfMapUtilization 은 map 라벨 별 분리 시리즈를 검증한다. starts 와 pod_bytes 두 map 이
// 동시 emit 될 때 라벨 충돌 없이 독립 추적되는지 가드한다.
func TestSetBpfMapUtilization(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(bpfMapUtilizationRatio)

	SetBpfMapUtilization("starts", 0.42)
	SetBpfMapUtilization("pod_bytes", 0.81)

	if got := testutil.ToFloat64(bpfMapUtilizationRatio.WithLabelValues("starts")); got != 0.42 {
		t.Errorf("starts=%v want 0.42", got)
	}
	if got := testutil.ToFloat64(bpfMapUtilizationRatio.WithLabelValues("pod_bytes")); got != 0.81 {
		t.Errorf("pod_bytes=%v want 0.81", got)
	}
}

// TestSetInformerSyncLag 은 단일 시리즈 gauge 의 set / overwrite 동작을 검증한다. 호출마다 누적
// 이 아닌 마지막 값으로 덮어쓰는 게 informer lag 의 의미 (즉시 staleness 가시화) 와 정합하다.
func TestSetInformerSyncLag(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(informerSyncLagSeconds)

	SetInformerSyncLag(12.5)
	if got := gaugeValue(t, informerSyncLagSeconds); got != 12.5 {
		t.Errorf("first set=%v want 12.5", got)
	}

	SetInformerSyncLag(3.0)
	if got := gaugeValue(t, informerSyncLagSeconds); got != 3.0 {
		t.Errorf("overwrite=%v want 3.0 (later set should replace earlier)", got)
	}
}

// TestSelfHealthMetricsRegister 는 Register 가 self-health 4 종을 모두 등록하는지 회귀 가드한다.
// 새 메트릭 추가가 누락된 채 layer 만 도입되면 운영에서 라벨 emit 자체가 dead-code 가 되므로
// 본 테스트가 등록 단계의 정합성을 보장한다.
func TestSelfHealthMetricsRegister(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	Register(reg)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}

	want := []string{
		"netobs_bpf_program_loaded",
		"netobs_bpf_ringbuf_drops_total",
		"netobs_bpf_map_utilization_ratio",
		"netobs_informer_sync_lag_seconds",
	}
	// Register 직후 metric emit 이 없는 메트릭은 Gather 에서 보이지 않으므로 setter 로 시리즈를 1
	// 건씩 만든 뒤 다시 gather 한다.
	SetBpfProgramLoaded("probe", true)
	AddBpfRingbufDrops(1)
	SetBpfMapUtilization("starts", 0.1)
	SetInformerSyncLag(1)
	mfs, err = reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names = make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, n := range want {
		if !names[n] {
			t.Errorf("metric %q missing after Register; have %s", n, strings.Join(keys(names), ", "))
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
