package podbytes

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/kube"
)

// fakeResolver는 단위 테스트용 PodResolver다. table에서 cgroup_id 기준 PodIdentity를 반환한다.
type fakeResolver struct {
	table map[uint64]kube.PodIdentity
}

func (f *fakeResolver) ResolveCgroup(cgroupID uint64) (kube.PodIdentity, bool) {
	id, ok := f.table[cgroupID]
	return id, ok
}

// TestDirectionLabel은 BPF enum 값과 Prometheus 라벨 문자열 매핑이 정확한지 검증한다. 알 수 없는
// 값이 들어왔을 때 unknown 으로 fallback 되어 카디널리티 안전 보장됨을 함께 확인한다.
func TestDirectionLabel(t *testing.T) {
	cases := []struct {
		in   uint8
		want string
	}{
		{0, dirEgress},
		{1, dirIngress},
		{2, dirUnknown},
		{255, dirUnknown},
	}
	for _, c := range cases {
		if got := directionLabel(c.in); got != c.want {
			t.Errorf("directionLabel(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestLayerLabel(t *testing.T) {
	cases := []struct {
		in   uint8
		want string
	}{
		{0, layerNIC},
		{1, layerL4},
		{7, layerUnknown},
		{255, layerUnknown},
	}
	for _, c := range cases {
		if got := layerLabel(c.in); got != c.want {
			t.Errorf("layerLabel(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestCollectorDisabledEmitsNothing은 PodMetricsEnabled=false 토글이 Collector를 완전히
// 비활성화하는지 검증한다. POD_METRICS_ENABLED=false 환경에서도 본 collector가 시리즈를 emit하면
// 안 된다는 이슈의 수용 조건과 정합한다.
func TestCollectorDisabledEmitsNothing(t *testing.T) {
	resolver := &fakeResolver{}
	c := New(resolver, "test-node", false)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, mf := range mfs {
		if got := len(mf.Metric); got != 0 {
			t.Errorf("metric %q emitted %d series under disabled state; want 0", mf.GetName(), got)
		}
	}
}

// TestCollectorNilMapEmitsNothing은 BPF map 주입 (SetMap) 전 scrape가 panic 없이 빈 결과를 반환하는지
// 검증한다. ebpfx.Run의 onReady가 호출되기 전 Prometheus가 scrape하는 startup race를 안전 처리하는 가드다.
func TestCollectorNilMapEmitsNothing(t *testing.T) {
	resolver := &fakeResolver{}
	c := New(resolver, "test-node", true)

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, mf := range mfs {
		if got := len(mf.Metric); got != 0 {
			t.Errorf("metric %q emitted %d series with nil bpf map; want 0", mf.GetName(), got)
		}
	}
}

// TestCollectorDescribe은 두 메트릭 (netobs_pod_bytes_total, netobs_pod_packets_total) 의 description이
// 정상 등록되는지 검증한다. Describe가 빈 결과를 내면 Prometheus가 collector를 인식하지 못해 시리즈가
// 영구적으로 누락되므로 startup 시점의 sanity check 차원에서 함께 가드한다.
func TestCollectorDescribe(t *testing.T) {
	c := New(&fakeResolver{}, "node-a", true)

	ch := make(chan *prometheus.Desc, 4)
	c.Describe(ch)
	close(ch)

	got := 0
	for d := range ch {
		if d == nil {
			t.Errorf("nil desc received")
		}
		got++
	}
	if got != 2 {
		t.Errorf("Describe emitted %d descs; want 2 (bytes + packets)", got)
	}
}
