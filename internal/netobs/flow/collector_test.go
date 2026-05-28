package flow

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/kube"
	ebpfx "netobs/internal/netobs/ebpf"
	"netobs/internal/netobs/metadata"
	"netobs/internal/netobs/metrics"
)

// fakeCgroup / fakeIP 는 두 인터페이스 의 테스트 더블 이다. 미리 셋팅 된 매핑 을 그대로 반환 한다.
type fakeCgroup struct {
	byCgroup map[uint64]kube.PodIdentity
}

func (f *fakeCgroup) ResolveCgroup(cgroup uint64) (kube.PodIdentity, bool) {
	p, ok := f.byCgroup[cgroup]
	return p, ok
}

type fakeIP struct {
	byIP map[string]kube.PodIdentity
}

func (f *fakeIP) ResolveIP(ip string) kube.PodIdentity {
	if p, ok := f.byIP[ip]; ok {
		return p
	}
	return kube.PodIdentity{}
}

// TestDirectionLabel 은 BPF enum (0=egress, 1=ingress) 의 라벨 매핑 회귀 가드 다.
func TestDirectionLabel(t *testing.T) {
	cases := []struct {
		v    uint8
		want string
	}{
		{0, "egress"},
		{1, "ingress"},
		{99, "unknown"},
	}
	for _, tc := range cases {
		got := directionLabel(tc.v)
		if got != tc.want {
			t.Errorf("directionLabel(%d)=%q want %q", tc.v, got, tc.want)
		}
	}
}

// TestCollector_DisabledOrNilGuardSkipsEmit 은 enabled=false 또는 guard nil 일 때 collector 가 어떤
// 시리즈 도 emit 하지 않는 회귀 가드 다. opt-in 안전 default 의 핵심 정책.
func TestCollector_DisabledOrNilGuardSkipsEmit(t *testing.T) {
	cgroup := &fakeCgroup{}

	// enabled=false
	c1 := New(cgroup, nil, metrics.NewFlowGuard([]string{"ns"}, 100), nil, "node1", false)
	if count := testutil.CollectAndCount(c1, "netobs_flow_bytes_total"); count != 0 {
		t.Errorf("enabled=false count=%d want 0", count)
	}

	// guard nil
	c2 := New(cgroup, nil, nil, nil, "node1", true)
	if count := testutil.CollectAndCount(c2, "netobs_flow_bytes_total"); count != 0 {
		t.Errorf("guard=nil count=%d want 0", count)
	}
}

// TestCollector_NoMapEmitsNothing 은 SetMap 호출 전 또는 nil map 일 때 collector 가 panic 없이 빈
// 결과 를 반환 하는 회귀 가드 다. startup race 안전.
func TestCollector_NoMapEmitsNothing(t *testing.T) {
	cgroup := &fakeCgroup{}
	c := New(cgroup, nil, metrics.NewFlowGuard([]string{"ns"}, 100), nil, "node1", true)
	if count := testutil.CollectAndCount(c, "netobs_flow_bytes_total"); count != 0 {
		t.Errorf("nil map count=%d want 0", count)
	}
}

// TestCollector_DescribeEmitsSingleDesc 는 본 collector 가 단일 desc 만 노출 하는지 회귀 가드 한다.
// Describe 단계 의 prometheus.Registerer 충돌 방지 패턴.
func TestCollector_DescribeEmitsSingleDesc(t *testing.T) {
	c := New(&fakeCgroup{}, nil, metrics.NewFlowGuard(nil, 100), nil, "node1", true)
	ch := make(chan *prometheus.Desc, 4)
	c.Describe(ch)
	close(ch)
	var got []string
	for d := range ch {
		got = append(got, d.String())
	}
	if len(got) != 1 {
		t.Errorf("desc count=%d want 1", len(got))
	}
	if !strings.Contains(got[0], "netobs_flow_bytes_total") {
		t.Errorf("desc=%q want contains netobs_flow_bytes_total", got[0])
	}
}

// TestCollector_DstClassifierIntegration 는 dstClassifier nil 시 dst 라벨 셋 두 칸 이 빈 문자열 로
// 채워 지는지 가드 한다. dst master switch 가 꺼진 운영 모드 의 회귀 가드.
func TestCollector_DstClassifierIntegration(t *testing.T) {
	c := New(&fakeCgroup{}, nil, metrics.NewFlowGuard(nil, 100), nil, "node1", true)
	// classifier nil 이면 emitEntry 내부 분기 가 빈 문자열 두 칸 으로 채운다. 실제 emit 까지 가는
	// 경로 는 BPF map 주입 이 필요해 단위 테스트 에서 직접 검증 어렵다. nil 분기 가 panic 없이 통과
	// 하는 자리 만 확보 한다.
	if c.dstClassifier != nil {
		t.Errorf("dstClassifier=%v want nil", c.dstClassifier)
	}
}

// TestCollector_DstClassifierEnabled 는 dstClassifier 가 enabled=true 로 주입 된 collector 의 dst
// classifier 가 호출 가능 한 상태인지 확인 하는 가드 다.
func TestCollector_DstClassifierEnabled(t *testing.T) {
	classifier := metadata.NewDstLabelClassifier(true, []string{"observability-test"})
	c := New(&fakeCgroup{}, &fakeIP{}, metrics.NewFlowGuard([]string{"observability-test"}, 100), classifier, "node1", true)
	if c.dstClassifier == nil {
		t.Errorf("dstClassifier=nil want non-nil")
	}
}

// podOf 는 PodIdentity 빌더 헬퍼. mergeEntry 단위 테스트가 IsPod() / PodUID 가드 통과 entry 를 쉽게
// 만들도록 IdentityClass=Pod 와 필수 필드 를 채운다.
func podOf(ns, name, uid, workload string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     ns,
		PodName:       name,
		PodUID:        uid,
		Workload:      workload,
		WorkloadKind:  "Deployment",
	}
}

// makeKey 는 BPF map iterate 입력 helper. 인자는 localIP, remoteIP, localPort, remotePort 형태 로
// BPF 측 raw 5-tuple 의미 와 정합 한다 (saddr=local, daddr=remote).
func makeKey(cgroupID uint64, localIPInt, remoteIPInt uint32, localPort, remotePort uint16, dir uint8) ebpfx.NetObsNetobsFlowKey {
	return ebpfx.NetObsNetobsFlowKey{
		CgroupId:  cgroupID,
		Saddr:     localIPInt,
		Daddr:     remoteIPInt,
		Sport:     localPort,
		Dport:     remotePort,
		Protocol:  6, // TCP
		Direction: dir,
	}
}

// ipBytes 는 LE 환경 가정 으로 a.b.c.d 형태 의 IPv4 를 BPF 가 저장 하는 uint32 (network byte order
// 의 native uint32 layout) 로 변환 한다. types.U32ToIPv4 와 정확히 역방향 이어야 단위 테스트 의 IP
// label 기대값 (a.b.c.d) 이 통과 한다.
func ipBytes(a, b, c, d byte) uint32 {
	return uint32(a) | uint32(b)<<8 | uint32(c)<<16 | uint32(d)<<24
}

// TestMergeEntry_EgressAssignsLocalAsSender 는 egress direction 의 entry 가 src=local Pod (cgroup 기반)
// 로 라벨 셋 을 가지고 dst 측 은 dstClassifier 로 채워지는 회귀 가드 다. swap 이 일어 나지 않 음을 함께
// 확인 한다.
func TestMergeEntry_EgressAssignsLocalAsSender(t *testing.T) {
	cgroupResolver := &fakeCgroup{byCgroup: map[uint64]kube.PodIdentity{
		111: podOf("ns-a", "client-x", "uid-client", "client"),
	}}
	ipResolver := &fakeIP{byIP: map[string]kube.PodIdentity{
		"10.0.0.2": podOf("ns-b", "server-x", "uid-server", "server"),
	}}
	classifier := metadata.NewDstLabelClassifier(true, []string{"ns-a"})
	c := New(cgroupResolver, ipResolver, metrics.NewFlowGuard([]string{"ns-a"}, 100), classifier, "node1", true)

	key := makeKey(111, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 0) // egress
	agg := map[aggKey]*aggValue{}
	c.mergeEntry(agg, key, ebpfx.NetObsNetobsFlowValue{Bytes: 1000})

	if len(agg) != 1 {
		t.Fatalf("agg size=%d want 1", len(agg))
	}
	var k aggKey
	var v *aggValue
	for kk, vv := range agg {
		k, v = kk, vv
	}
	if k.srcIP != "10.0.0.1" || k.dstIP != "10.0.0.2" {
		t.Errorf("egress (src,dst)=(%s,%s) want (10.0.0.1,10.0.0.2)", k.srcIP, k.dstIP)
	}
	if v.srcUID != "uid-client" || v.srcPod != "client-x" {
		t.Errorf("src 라벨 셋=(%s,%s) want (uid-client,client-x)", v.srcUID, v.srcPod)
	}
}

// TestMergeEntry_IngressSwapsSrcDst 는 ingress direction 의 entry 가 src/dst swap 으로 src=remote
// sender (IP resolve), dst=local receiver (cgroup) 로 채워지는 회귀 가드 다. emit_rcv_event 패턴 과
// 의 라벨 의미 정합을 단위 테스트로 lock-in 한다.
func TestMergeEntry_IngressSwapsSrcDst(t *testing.T) {
	cgroupResolver := &fakeCgroup{byCgroup: map[uint64]kube.PodIdentity{
		222: podOf("ns-a", "client-x", "uid-client", "client"),
	}}
	ipResolver := &fakeIP{byIP: map[string]kube.PodIdentity{
		"10.0.0.2": podOf("ns-b", "server-x", "uid-server", "server"),
	}}
	classifier := metadata.NewDstLabelClassifier(true, []string{"ns-a"})
	c := New(cgroupResolver, ipResolver, metrics.NewFlowGuard([]string{"ns-a"}, 100), classifier, "node1", true)

	// BPF raw key (local=10.0.0.1, remote=10.0.0.2, direction=ingress).
	key := makeKey(222, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 1)
	agg := map[aggKey]*aggValue{}
	c.mergeEntry(agg, key, ebpfx.NetObsNetobsFlowValue{Bytes: 500})

	if len(agg) != 1 {
		t.Fatalf("agg size=%d want 1", len(agg))
	}
	var k aggKey
	var v *aggValue
	for kk, vv := range agg {
		k, v = kk, vv
	}
	// swap 후 src=remote (10.0.0.2), dst=local (10.0.0.1)
	if k.srcIP != "10.0.0.2" || k.dstIP != "10.0.0.1" {
		t.Errorf("ingress swap (src,dst)=(%s,%s) want (10.0.0.2,10.0.0.1)", k.srcIP, k.dstIP)
	}
	if k.srcPort != 80 || k.dstPort != 1234 {
		t.Errorf("ingress port swap (sport,dport)=(%d,%d) want (80,1234)", k.srcPort, k.dstPort)
	}
	// src 라벨 셋 은 IP resolve 로 server pod identity, dst 라벨 셋 은 cgroup 의 client pod identity
	if v.srcUID != "uid-server" || v.srcPod != "server-x" {
		t.Errorf("ingress src 라벨 셋=(%s,%s) want (uid-server,server-x)", v.srcUID, v.srcPod)
	}
	if v.dstUID != "uid-client" {
		t.Errorf("ingress dst_pod_uid=%s want uid-client (localPod)", v.dstUID)
	}
}

// TestMergeEntry_GuardChecksLocalNamespace 는 ingress 의 src 라벨 이 swap 으로 remote namespace 가
// 되더라도 FlowGuard.Admit 가 localPod 의 namespace 로 가드 검사를 수행 해 "본 노드 의 allow-list pod
// 의 양 방향 flow" 가 일관 capture 되는지 검증한다.
func TestMergeEntry_GuardChecksLocalNamespace(t *testing.T) {
	cgroupResolver := &fakeCgroup{byCgroup: map[uint64]kube.PodIdentity{
		333: podOf("ns-a", "client-x", "uid-client", "client"),
	}}
	// remote IP 는 ns-b 의 pod 로 resolve 된다 (swap 후 src_namespace=ns-b 가 됨).
	ipResolver := &fakeIP{byIP: map[string]kube.PodIdentity{
		"10.0.0.2": podOf("ns-b", "server-x", "uid-server", "server"),
	}}
	// guard allow-list = [ns-a]. local pod 는 ns-a → 양 방향 모두 admit 되어야 한다.
	c := New(cgroupResolver, ipResolver, metrics.NewFlowGuard([]string{"ns-a"}, 100), nil, "node1", true)

	keyEgress := makeKey(333, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 0)
	keyIngress := makeKey(333, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 1)
	agg := map[aggKey]*aggValue{}
	c.mergeEntry(agg, keyEgress, ebpfx.NetObsNetobsFlowValue{Bytes: 100})
	c.mergeEntry(agg, keyIngress, ebpfx.NetObsNetobsFlowValue{Bytes: 200})

	if len(agg) != 2 {
		t.Errorf("egress + ingress agg size=%d want 2 (local namespace 가드 미동작)", len(agg))
	}
}

// TestMergeEntry_DedupsMultiCgroupSamePodUID 는 동일 PodUID 의 두 cgroup_id entry 가 동일 5-tuple +
// direction 을 가질 때 단일 aggregate 항목 에 합산 되는지 검증 한다. Prometheus 의 동일 라벨 셋 중복
// 시리즈 거부 회피 의 핵심 가드 다.
func TestMergeEntry_DedupsMultiCgroupSamePodUID(t *testing.T) {
	cgroupResolver := &fakeCgroup{byCgroup: map[uint64]kube.PodIdentity{
		// 동일 PodUID 의 두 다른 cgroup (multi-container 또는 pod 내 다중 cgroup).
		1001: podOf("ns-a", "client-x", "uid-client", "client"),
		1002: podOf("ns-a", "client-x", "uid-client", "client"),
	}}
	c := New(cgroupResolver, &fakeIP{}, metrics.NewFlowGuard([]string{"ns-a"}, 100), nil, "node1", true)

	k1 := makeKey(1001, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 0)
	k2 := makeKey(1002, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 0)
	agg := map[aggKey]*aggValue{}
	c.mergeEntry(agg, k1, ebpfx.NetObsNetobsFlowValue{Bytes: 100})
	c.mergeEntry(agg, k2, ebpfx.NetObsNetobsFlowValue{Bytes: 200})

	if len(agg) != 1 {
		t.Fatalf("multi-cgroup 합산 후 agg size=%d want 1", len(agg))
	}
	for _, v := range agg {
		if v.bytes != 300 {
			t.Errorf("bytes=%d want 300 (100+200 합산)", v.bytes)
		}
	}
}

// TestMergeEntry_SkipsUnresolvedCgroup 은 cgroup_id 가 resolver 에 없 거나 IsPod() 가 false 이거나
// PodUID 가 비어 있는 entry 가 모두 skip 되는 회귀 가드 다. informer race / external pod 등의 startup
// 윈도우 안전성 가드.
func TestMergeEntry_SkipsUnresolvedCgroup(t *testing.T) {
	cgroupResolver := &fakeCgroup{byCgroup: map[uint64]kube.PodIdentity{
		// PodUID 비어 있는 entry (informer race 윈도우)
		2001: {IdentityClass: kube.IdentityClassPod, Namespace: "ns-a", PodName: "client-x"},
		// IsPod() == false 인 entry
		2002: {IdentityClass: kube.IdentityClassUnresolved},
	}}
	c := New(cgroupResolver, &fakeIP{}, metrics.NewFlowGuard([]string{"ns-a"}, 100), nil, "node1", true)

	// resolver 에 없는 cgroup
	keyMissing := makeKey(9999, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 0)
	keyEmptyUID := makeKey(2001, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 0)
	keyNotPod := makeKey(2002, ipBytes(10, 0, 0, 1), ipBytes(10, 0, 0, 2), 1234, 80, 0)
	agg := map[aggKey]*aggValue{}
	c.mergeEntry(agg, keyMissing, ebpfx.NetObsNetobsFlowValue{Bytes: 100})
	c.mergeEntry(agg, keyEmptyUID, ebpfx.NetObsNetobsFlowValue{Bytes: 100})
	c.mergeEntry(agg, keyNotPod, ebpfx.NetObsNetobsFlowValue{Bytes: 100})

	if len(agg) != 0 {
		t.Errorf("agg size=%d want 0 (모든 unresolved entry 가 skip 되어야 함)", len(agg))
	}
}
