package podbytes

import (
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/kube"
	ebpfx "netobs/internal/netobs/ebpf"
)

func newPod(uid, ns, name string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     ns,
		PodName:       name,
		PodUID:        uid,
	}
}

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

// TestMergeEntryAggregatesByPodUID는 한 Pod이 여러 cgroup_id로 학습됐을 때 (container 단위, 다중
// 프로세스 등) 동일 (direction, layer, podUID) 키 아래로 bytes/packets가 합산되어 단일 시리즈로
// emit되는지 검증한다. 0.3.8 dev 검증에서 본 합산이 누락돼 Prometheus "동일 라벨 셋 중복 시리즈"
// 오류가 났던 회귀에 대한 가드이며, BPF map mock 없이 합산 로직을 격리해 테스트한다.
func TestMergeEntryAggregatesByPodUID(t *testing.T) {
	pod := newPod("uid-1", "ns", "p1")
	resolver := &fakeResolver{table: map[uint64]kube.PodIdentity{
		100: pod,
		200: pod,
	}}
	agg := make(map[aggKey]*aggValue)

	// 같은 PodUID로 해상되는 두 개의 cgroup_id entry를 합산 대상으로 넣는다.
	mergeEntry(agg, ebpfx.NetObsNetobsPodBytesKey{CgroupId: 100, Direction: 0, Layer: 0},
		[]ebpfx.NetObsNetobsPodBytesValue{{Bytes: 1000, Packets: 3}, {Bytes: 500, Packets: 2}}, resolver)
	mergeEntry(agg, ebpfx.NetObsNetobsPodBytesKey{CgroupId: 200, Direction: 0, Layer: 0},
		[]ebpfx.NetObsNetobsPodBytesValue{{Bytes: 200, Packets: 1}}, resolver)

	if got := len(agg); got != 1 {
		t.Fatalf("agg size=%d want 1 (entries should merge into single aggKey)", got)
	}
	ak := aggKey{direction: 0, layer: 0, podUID: "uid-1"}
	av, ok := agg[ak]
	if !ok {
		t.Fatalf("aggKey %+v missing from agg", ak)
	}
	if av.bytes != 1700 || av.packets != 6 {
		t.Errorf("agg bytes=%d packets=%d want bytes=1700 packets=6", av.bytes, av.packets)
	}
	if av.pod.PodName != "p1" {
		t.Errorf("agg pod.PodName=%q want p1", av.pod.PodName)
	}
}

// TestMergeEntrySkipsUnknownCgroup는 enricher 캐시에 학습되지 않은 cgroup_id entry가 agg에 반영되지
// 않고 조용히 skip되는지 검증한다. event 흐름으로 캐시가 채워지면 다음 scrape에서 자연 emit되므로
// scrape 자체를 실패시키지 않는다.
func TestMergeEntrySkipsUnknownCgroup(t *testing.T) {
	resolver := &fakeResolver{table: map[uint64]kube.PodIdentity{}}
	agg := make(map[aggKey]*aggValue)

	mergeEntry(agg, ebpfx.NetObsNetobsPodBytesKey{CgroupId: 999, Direction: 0, Layer: 0},
		[]ebpfx.NetObsNetobsPodBytesValue{{Bytes: 1000, Packets: 1}}, resolver)

	if got := len(agg); got != 0 {
		t.Errorf("agg size=%d want 0 (unknown cgroup_id must be skipped)", got)
	}
}

// TestMergeEntrySkipsEmptyPodUID는 IsPod=true 이지만 PodUID가 빈 문자열인 entry를 skip하는지
// 검증한다. informer race로 잠시 발생 가능한 케이스이며, 가드 없이 통과시키면 서로 다른 Pod 두 개
// 이상이 동일 빈 aggKey 아래로 합쳐져 첫 entry의 라벨 (namespace, podName) 로 emit되는 라벨
// cross-pollination 결함이 생긴다.
func TestMergeEntrySkipsEmptyPodUID(t *testing.T) {
	podA := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ns", PodName: "a"}
	podB := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "ns", PodName: "b"}
	resolver := &fakeResolver{table: map[uint64]kube.PodIdentity{
		100: podA,
		200: podB,
	}}
	agg := make(map[aggKey]*aggValue)

	mergeEntry(agg, ebpfx.NetObsNetobsPodBytesKey{CgroupId: 100, Direction: 0, Layer: 0},
		[]ebpfx.NetObsNetobsPodBytesValue{{Bytes: 1000, Packets: 1}}, resolver)
	mergeEntry(agg, ebpfx.NetObsNetobsPodBytesKey{CgroupId: 200, Direction: 0, Layer: 0},
		[]ebpfx.NetObsNetobsPodBytesValue{{Bytes: 2000, Packets: 2}}, resolver)

	if got := len(agg); got != 0 {
		t.Errorf("agg size=%d want 0 (empty PodUID must be skipped to prevent label cross-pollination)", got)
	}
}

// TestMergeEntrySkipsNonPodIdentity는 resolver가 service/node 등 Pod 이외 IdentityClass를 반환한
// entry를 skip하는지 검증한다. pod_bytes는 Pod 단위 카운터이므로 비-Pod 정체성은 라벨 채우기가
// 불가능해 emit하지 않는다.
func TestMergeEntrySkipsNonPodIdentity(t *testing.T) {
	resolver := &fakeResolver{table: map[uint64]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassService, Namespace: "default"},
	}}
	agg := make(map[aggKey]*aggValue)

	mergeEntry(agg, ebpfx.NetObsNetobsPodBytesKey{CgroupId: 100, Direction: 0, Layer: 0},
		[]ebpfx.NetObsNetobsPodBytesValue{{Bytes: 1000, Packets: 1}}, resolver)

	if got := len(agg); got != 0 {
		t.Errorf("agg size=%d want 0 (non-Pod identity must be skipped)", got)
	}
}

// TestMergeEntrySeparatesByDirectionAndLayer는 같은 PodUID라도 (direction, layer) 조합이 다르면
// 별도 aggKey로 분리되어 4개 (egress/ingress × nic/l4) 시리즈가 독립적으로 누적되는지 검증한다.
func TestMergeEntrySeparatesByDirectionAndLayer(t *testing.T) {
	pod := newPod("uid-1", "ns", "p1")
	resolver := &fakeResolver{table: map[uint64]kube.PodIdentity{100: pod}}
	agg := make(map[aggKey]*aggValue)

	cases := []struct{ dir, layer uint8 }{
		{0, 0}, {0, 1}, {1, 0}, {1, 1},
	}
	for _, c := range cases {
		mergeEntry(agg, ebpfx.NetObsNetobsPodBytesKey{CgroupId: 100, Direction: c.dir, Layer: c.layer},
			[]ebpfx.NetObsNetobsPodBytesValue{{Bytes: 100, Packets: 1}}, resolver)
	}
	if got := len(agg); got != 4 {
		t.Errorf("agg size=%d want 4 (each (direction, layer) must be separate aggKey)", got)
	}
}

// TestEmitFromAggSkipsL4Packets는 BPF 측 packets_delta=0 규약 (L4 hook은 packets 누적 0) 하에서
// Collector가 packets > 0 인 entry (NIC layer) 만 packets 시리즈를 emit하고, L4 layer entry는
// bytes만 emit하는지 검증한다. 본 가드는 메트릭 이름의 의미 (packets = 실제 패킷 수) 와 emit
// 카디널리티 (L4 layer의 noise-only 0 시리즈 제거) 두 가지를 동시에 보호한다.
func TestEmitFromAggSkipsL4Packets(t *testing.T) {
	nicPod := newPod("uid-nic", "ns", "p-nic")
	l4Pod := newPod("uid-l4", "ns", "p-l4")
	agg := map[aggKey]*aggValue{
		{direction: 0, layer: 0, podUID: "uid-nic"}: {bytes: 5000, packets: 7, pod: nicPod},
		{direction: 0, layer: 1, podUID: "uid-l4"}:  {bytes: 9000, packets: 0, pod: l4Pod},
	}

	c := New(&fakeResolver{}, "node-a", true)
	ch := make(chan prometheus.Metric, 8)
	c.emitFromAgg(agg, ch)
	close(ch)

	var bytesCount, packetsCount int
	for m := range ch {
		fqName := m.Desc().String()
		switch {
		case containsName(fqName, "netobs_pod_bytes_total"):
			bytesCount++
		case containsName(fqName, "netobs_pod_packets_total"):
			packetsCount++
		}
	}
	if bytesCount != 2 {
		t.Errorf("bytes series=%d want 2 (one per agg entry, regardless of layer)", bytesCount)
	}
	if packetsCount != 1 {
		t.Errorf("packets series=%d want 1 (L4 entry with packets=0 must be skipped)", packetsCount)
	}
}

func containsName(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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

// TestCollector_ConcurrentSetMapAndCollectRace 는 #107 audit 의 회귀 가드 다. flow.Collector 의 동등
// 패턴 (atomic.Pointer 기반 SetMap-once + Describe / Load 동시 read) 의 race detector 회귀 차단 영구 가드.
func TestCollector_ConcurrentSetMapAndCollectRace(t *testing.T) {
	c := New(&fakeResolver{table: map[uint64]kube.PodIdentity{}}, "node1", true)

	const workers = 16
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.SetMap(nil)
			}
		}()
	}
	// Reader goroutine N 개: Collect 호출 로 내부 atomic.Pointer.Load 진입 (bpfMap nil 분기 까지 도달 해
	// race window 표면적 확보). Describe 는 c.bytesDesc / c.packetsDesc 만 emit 하고 bpfMap 을 참조
	// 하지 않 으므로 본 테스트 의 race detector 표면적 확보 의도 와 무관 하다. drain goroutine 으로
	// channel hang 회피.
	metricCh := make(chan prometheus.Metric, 4096)
	drainDone := make(chan struct{})
	go func() {
		for range metricCh {
		}
		close(drainDone)
	}()
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				c.Collect(metricCh)
			}
		}()
	}
	wg.Wait()
	close(metricCh)
	<-drainDone
}
