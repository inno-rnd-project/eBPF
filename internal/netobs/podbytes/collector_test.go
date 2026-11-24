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
// hintOnly에 든 cgroup_id는 스캐너가 모르고 힌트로만 풀리는 것으로 본다.
type fakeResolver struct {
	table    map[uint64]kube.PodIdentity
	hintOnly map[uint64]bool
}

func (f *fakeResolver) ResolveCgroupScanned(cgroupID uint64) (kube.PodIdentity, bool) {
	if f.hintOnly[cgroupID] {
		return kube.PodIdentity{}, false
	}
	return f.ResolveCgroup(cgroupID)
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

func ent(cg uint64, dir, layer uint8, bytes, packets uint64) mapEntry {
	return mapEntry{key: ebpfx.NetObsNetobsPodBytesKey{CgroupId: cg, Direction: dir, Layer: layer}, bytes: bytes, packets: packets}
}

// TestNewMapEntrySumsPerCPU는 PERCPU 맵의 CPU별 슬롯이 모두 더해지는지 검증한다.
func TestNewMapEntrySumsPerCPU(t *testing.T) {
	key := ebpfx.NetObsNetobsPodBytesKey{CgroupId: 100, Direction: 1, Layer: 0}
	e := newMapEntry(key, []ebpfx.NetObsNetobsPodBytesValue{{Bytes: 1000, Packets: 3}, {Bytes: 500, Packets: 2}, {}})
	if e.key != key || e.bytes != 1500 || e.packets != 5 {
		t.Errorf("entry=%+v want key=%+v bytes=1500 packets=5", e, key)
	}
}

// TestAggregateByPodUID는 한 pod의 여러 cgroup_id entry가 (direction, layer, podUID) 키 아래로 합산되어
// 단일 시리즈가 되는지 검증한다. 합산이 빠지면 Prometheus가 동일 라벨 셋 중복 시리즈로 거부한다.
func TestAggregateByPodUID(t *testing.T) {
	pod := newPod("uid-1", "ns", "p1")
	c := New(&fakeResolver{table: map[uint64]kube.PodIdentity{100: pod, 200: pod}}, "node-a", true)

	agg := c.aggregate([]mapEntry{ent(100, 0, 0, 1500, 5), ent(200, 0, 0, 200, 1)})

	if got := len(agg); got != 1 {
		t.Fatalf("agg size=%d want 1", got)
	}
	av := agg[aggKey{direction: 0, layer: 0, podUID: "uid-1"}]
	if av == nil || av.bytes != 1700 || av.packets != 6 || av.pod.PodName != "p1" {
		t.Errorf("agg=%+v want bytes=1700 packets=6 pod=p1", av)
	}
}

// TestAggregateSkipsUnresolved는 pod로 풀리지 않는 entry를 버리는지 검증한다. 대상은 스캐너가 모르는
// cgroup(호스트 프로세스), pod가 아닌 identity, UID가 빈 pod다. UID가 빈 pod 둘을 통과시키면 같은 빈
// 키로 합쳐져 첫 entry의 라벨로 나간다.
func TestAggregateSkipsUnresolved(t *testing.T) {
	c := New(&fakeResolver{table: map[uint64]kube.PodIdentity{
		100: {IdentityClass: kube.IdentityClassService, Namespace: "default"},
		200: {IdentityClass: kube.IdentityClassPod, Namespace: "ns", PodName: "a"},
		300: {IdentityClass: kube.IdentityClassPod, Namespace: "ns", PodName: "b"},
	}}, "node-a", true)

	agg := c.aggregate([]mapEntry{ent(100, 0, 0, 1000, 1), ent(200, 0, 0, 1000, 1), ent(300, 0, 0, 2000, 2), ent(999, 0, 0, 1000, 1)})

	if got := len(agg); got != 0 {
		t.Errorf("agg size=%d want 0", got)
	}
	if got := len(c.known); got != 0 {
		t.Errorf("known size=%d want 0", got)
	}
}

// TestAggregateSeparatesByDirectionAndLayer는 같은 pod라도 (direction, layer) 조합마다 시리즈가
// 따로 누적되는지 검증한다.
func TestAggregateSeparatesByDirectionAndLayer(t *testing.T) {
	c := New(&fakeResolver{table: map[uint64]kube.PodIdentity{100: newPod("uid-1", "ns", "p1")}}, "node-a", true)

	agg := c.aggregate([]mapEntry{ent(100, 0, 0, 100, 1), ent(100, 0, 1, 100, 0), ent(100, 1, 0, 100, 1), ent(100, 1, 1, 100, 0)})

	if got := len(agg); got != 4 {
		t.Errorf("agg size=%d want 4", got)
	}
}

// TestAggregateKeepsRestartedContainer는 컨테이너가 재시작해 옛 cgroup이 풀리지 않게 돼도 pod 합계가
// 줄지 않는지 검증한다. pod가 사라지면 시리즈가 사라지고, 한 번도 풀린 적 없는 cgroup은 pod가 살아
// 있어도 합산하지 않는다.
func TestAggregateKeepsRestartedContainer(t *testing.T) {
	pod := newPod("uid-1", "ns", "p1")
	resolver := &fakeResolver{table: map[uint64]kube.PodIdentity{1: pod, 2: pod}}
	c := New(resolver, "node-a", true)
	ak := aggKey{direction: 0, layer: 1, podUID: "uid-1"}
	entries := []mapEntry{ent(1, 0, 1, 200, 0), ent(2, 0, 1, 50000, 0), ent(9, 0, 1, 77, 0)}

	if got := c.aggregate(entries)[ak].bytes; got != 50200 {
		t.Fatalf("bytes=%d want 50200", got)
	}

	// 컨테이너(cgroup 1) 재시작: 옛 cgroup은 풀리지 않고 새 cgroup 3이 생긴다.
	delete(resolver.table, 1)
	resolver.table[3] = pod
	entries = append(entries, ent(3, 0, 1, 7, 0))
	if got := c.aggregate(entries)[ak].bytes; got != 50207 {
		t.Errorf("재시작 뒤 bytes=%d want 50207", got)
	}

	// pod 삭제: 어떤 cgroup도 풀리지 않으면 시리즈가 사라진다.
	delete(resolver.table, 2)
	delete(resolver.table, 3)
	if got := len(c.aggregate(entries)); got != 0 {
		t.Errorf("pod 삭제 뒤 agg size=%d want 0", got)
	}

	// BPF 맵에서 evict된 cgroup은 known에서도 지운다.
	c.aggregate(entries[1:])
	if _, ok := c.known[1]; ok {
		t.Errorf("맵에서 빠진 cgroup 1이 known에 남음")
	}
	if _, ok := c.known[2]; !ok {
		t.Errorf("맵에 남은 cgroup 2가 known에서 지워짐")
	}
}

// TestAggregateForgetsHintOnlyCgroup은 스캐너 없이 힌트로만 풀린 cgroup을 기억하지 않는지 검증한다.
// 힌트가 만료돼 풀리지 않게 되면, pod의 다른 cgroup이 살아 있어도 그 누적분은 합계에서 빠진다.
func TestAggregateForgetsHintOnlyCgroup(t *testing.T) {
	pod := newPod("uid-1", "ns", "p1")
	resolver := &fakeResolver{table: map[uint64]kube.PodIdentity{1: pod, 2: pod}, hintOnly: map[uint64]bool{1: true, 2: true}}
	c := New(resolver, "node-a", true)
	ak := aggKey{direction: 0, layer: 1, podUID: "uid-1"}
	entries := []mapEntry{ent(1, 0, 1, 200, 0), ent(2, 0, 1, 50000, 0)}

	if got := c.aggregate(entries)[ak].bytes; got != 50200 {
		t.Fatalf("bytes=%d want 50200", got)
	}
	delete(resolver.table, 1)
	if got := c.aggregate(entries)[ak].bytes; got != 50000 {
		t.Errorf("힌트 만료 뒤 bytes=%d want 50000", got)
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
