package podretrans

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"netobs/internal/kube"
	ebpfx "netobs/internal/netobs/ebpf"
)

type fakeResolver map[uint64]kube.PodIdentity

func (f fakeResolver) ResolveCgroupScanned(cgroupID uint64) (kube.PodIdentity, bool) {
	id, ok := f[cgroupID]
	return id, ok
}

func newPod(uid, name string) kube.PodIdentity {
	return kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ns", PodName: name, PodUID: uid}
}

func entry(cg uint64, kind uint8) ebpfx.NetObsNetobsPodRetransKey {
	return ebpfx.NetObsNetobsPodRetransKey{CgroupId: cg, Kind: kind}
}

// emitted는 emitFromAgg 결과를 "메트릭|kind|pod" 키의 값 맵으로 모은다.
func emitted(t *testing.T, c *Collector, agg map[aggKey]*aggValue) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 32)
	c.emitFromAgg(agg, ch)
	close(ch)
	out := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("metric write: %v", err)
		}
		labels := map[string]string{}
		for _, lp := range pb.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		name := "segments"
		if strings.Contains(m.Desc().String(), "netobs_pod_retrans_bytes_total") {
			name = "bytes"
		}
		if labels["node"] != "node-a" || labels["src_namespace"] != "ns" {
			t.Errorf("labels=%v", labels)
		}
		out[name+"|"+labels["kind"]+"|"+labels["src_pod"]] = pb.GetCounter().GetValue()
	}
	return out
}

func ent(cg uint64, kind uint8, segs, b uint64) mapEntry {
	return mapEntry{key: entry(cg, kind), val: ebpfx.NetObsNetobsPodRetransValue{Segs: segs, Bytes: b}}
}

// TestAggregateAndEmit은 같은 pod의 여러 cgroup entry가 kind별로 합산되고, 스캐너가 pod로 풀지 못한
// entry(호스트 cgroup, 비 pod, 빈 UID)는 버려지며, SYN-ACK은 bytes 시리즈 없이 세그먼트만 나가는지
// 검증한다.
func TestAggregateAndEmit(t *testing.T) {
	resolver := fakeResolver{
		1: newPod("u1", "p1"),
		2: newPod("u1", "p1"), // 같은 pod의 다른 container cgroup
		3: newPod("u2", "p2"),
		4: {IdentityClass: kube.IdentityClassNode, NodeName: "node-a"},
		5: {IdentityClass: kube.IdentityClassPod, Namespace: "ns", PodName: "racing"},
	}
	c := New(resolver, "node-a", true)
	got := emitted(t, c, c.aggregate([]mapEntry{
		ent(1, 0, 10, 14000),
		ent(2, 0, 5, 7000),
		ent(1, 1, 2, 0),
		ent(3, 2, 4, 0),
		ent(4, 0, 99, 99),  // node identity
		ent(5, 0, 99, 99),  // UID 미채움
		ent(77, 0, 99, 99), // 스캐너 미등록 호스트 cgroup
	}))
	want := map[string]float64{
		"segments|data|p1":   15,
		"bytes|data|p1":      21000,
		"segments|syn|p1":    2,
		"bytes|syn|p1":       0,
		"segments|synack|p2": 4,
	}
	assertSeries(t, got, want)
}

// TestAggregateKeepsRestartedContainer는 컨테이너가 재시작해 옛 cgroup이 스캐너 테이블에서 빠져도 pod의
// 다른 cgroup이 살아 있는 동안 옛 entry가 합계에 남아 counter가 줄지 않는지, pod가 사라지면 시리즈가
// 감소 없이 사라지는지, BPF 맵에서 빠진 cgroup은 known에서도 지워지는지 검증한다.
func TestAggregateKeepsRestartedContainer(t *testing.T) {
	resolver := fakeResolver{1: newPod("u1", "p1"), 2: newPod("u1", "p1")}
	c := New(resolver, "node-a", true)
	entries := []mapEntry{ent(1, 0, 200, 0), ent(2, 0, 50000, 0)}

	assertSeries(t, emitted(t, c, c.aggregate(entries)), map[string]float64{"segments|data|p1": 50200, "bytes|data|p1": 0})

	// app 컨테이너(cgroup 1) 재시작: 옛 scope가 사라지고 새 scope(cgroup 3)가 생긴다.
	delete(resolver, 1)
	resolver[3] = newPod("u1", "p1")
	entries = append(entries, ent(3, 0, 7, 0))
	assertSeries(t, emitted(t, c, c.aggregate(entries)), map[string]float64{"segments|data|p1": 50207, "bytes|data|p1": 0})

	// pod 삭제: 어떤 cgroup도 풀리지 않으면 시리즈가 사라진다.
	delete(resolver, 2)
	delete(resolver, 3)
	assertSeries(t, emitted(t, c, c.aggregate(entries)), map[string]float64{})

	// BPF 맵에서 evict된 cgroup은 known에서도 지운다.
	c.aggregate(entries[1:])
	if _, ok := c.known[1]; ok {
		t.Errorf("맵에서 빠진 cgroup 1이 known에 남음")
	}
	if _, ok := c.known[2]; !ok {
		t.Errorf("맵에 남은 cgroup 2가 known에서 지워짐")
	}
}

func assertSeries(t *testing.T, got, want map[string]float64) {
	t.Helper()
	if len(got) != len(want) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("emitted %v want %d series", keys, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%v want %v", k, got[k], v)
		}
	}
}

// TestCollectGates는 POD_METRICS_ENABLED=false와 맵 주입 전 scrape가 아무 시리즈도 내지 않는지 검증한다.
func TestCollectGates(t *testing.T) {
	for _, c := range []*Collector{New(fakeResolver{}, "node-a", false), New(fakeResolver{}, "node-a", true)} {
		ch := make(chan prometheus.Metric, 4)
		c.Collect(ch)
		close(ch)
		if n := len(ch); n != 0 {
			t.Errorf("enabled=%v emitted %d series without map", c.enabled, n)
		}
	}
}

func TestKindLabel(t *testing.T) {
	for in, want := range map[uint8]string{0: kindData, 1: kindSyn, 2: kindSynack, 3: kindUnknown, 255: kindUnknown} {
		if got := kindLabel(in); got != want {
			t.Errorf("kindLabel(%d)=%q want %q", in, got, want)
		}
	}
}
