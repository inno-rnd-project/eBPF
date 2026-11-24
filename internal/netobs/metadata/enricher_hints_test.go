package metadata

import (
	"testing"
	"time"

	"netobs/internal/kube"
	"netobs/internal/netobs/types"
)

// TestApplyRuntimeHints_IngressRoutesToDst 는 #65 의 receive path 가 BPF 측에서 src/dst 를 swap 해
// dst 가 수신 Pod 가 되는 ingress event 에서 cgroup_id 와 Ifindex 힌트가 dst 쪽에 적재되는지 확인
// 한다. 외부 peer 가 송신자인 케이스 (src 가 PodIdentity 미해석) 에서 힌트가 src 로 잘못 흘러 가면
// peer 가 수신 Pod 신원으로 덮여써지는 회귀가 일어나므로 본 가드가 필요하다.
func TestApplyRuntimeHints_IngressRoutesToDst(t *testing.T) {
	e := &Enricher{
		runtimeByCgroup:   make(map[uint64]runtimeCacheEntry),
		runtimeByIfindex:  make(map[uint32]runtimeCacheEntry),
		runtimeTTL:        time.Minute,
		runtimeSweepEvery: time.Minute,
	}

	recv := kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     "ns",
		PodName:       "recv-pod",
	}
	now := time.Now()
	e.rememberCgroupHint(0xdeadbeef, recv, now)
	e.rememberIfindexHint(42, recv, now)

	ev := types.Event{
		Stage:    types.StageRcvDemux,
		CgroupID: 0xdeadbeef,
		Ifindex:  42,
	}
	srcExternal := kube.PodIdentity{IdentityClass: kube.IdentityClassExternal}
	dstUnresolved := kube.PodIdentity{IdentityClass: kube.IdentityClassUnresolved}

	src, dst := e.applyRuntimeHints(ev, "8.8.8.8", "10.0.0.10", srcExternal, dstUnresolved, now)

	if src.IsPod() {
		t.Errorf("ingress event 의 src 가 Pod 로 잘못 덮여써짐: %+v (peer 가 수신 Pod 로 둔갑)", src)
	}
	if !dst.IsPod() {
		t.Fatalf("ingress event 의 dst 가 cgroup/ifindex 힌트로 수신 Pod 로 해석되지 않음: %+v", dst)
	}
	if dst.PodName != "recv-pod" {
		t.Errorf("dst.PodName=%q want recv-pod", dst.PodName)
	}
}

// TestApplyRuntimeHints_EgressRoutesToSrc 는 기존 send path 의 cgroup / Ifindex → src 힌트 동작이
// direction 분기 도입 이후에도 깨지지 않는지 회귀 가드한다.
func TestApplyRuntimeHints_EgressRoutesToSrc(t *testing.T) {
	e := &Enricher{
		runtimeByCgroup:   make(map[uint64]runtimeCacheEntry),
		runtimeByIfindex:  make(map[uint32]runtimeCacheEntry),
		runtimeTTL:        time.Minute,
		runtimeSweepEvery: time.Minute,
	}

	sender := kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     "ns",
		PodName:       "send-pod",
	}
	now := time.Now()
	e.rememberCgroupHint(0xcafef00d, sender, now)
	e.rememberIfindexHint(7, sender, now)

	ev := types.Event{
		Stage:    types.StageSendmsgRet,
		CgroupID: 0xcafef00d,
		Ifindex:  7,
	}
	srcUnresolved := kube.PodIdentity{IdentityClass: kube.IdentityClassUnresolved}
	dstExternal := kube.PodIdentity{IdentityClass: kube.IdentityClassExternal}

	src, dst := e.applyRuntimeHints(ev, "10.0.0.10", "8.8.8.8", srcUnresolved, dstExternal, now)

	if !src.IsPod() {
		t.Fatalf("egress event 의 src 가 cgroup/ifindex 힌트로 송신 Pod 로 해석되지 않음: %+v", src)
	}
	if src.PodName != "send-pod" {
		t.Errorf("src.PodName=%q want send-pod", src.PodName)
	}
	if dst.IsPod() {
		t.Errorf("egress event 의 dst 가 Pod 로 잘못 덮여써짐: %+v", dst)
	}
}

// TestRememberRuntimeHints_HostNetworkSkipsIfindexHint 는 hostNetwork pod 의 이벤트가 공유 host
// 인터페이스 ifindex 를 힌트로 학습하지 않는 것을 검증한다 (#321). 학습되면 kubelet 등 host
// 프로세스 트래픽이 ifindex 힌트를 타고 이 pod 로 오귀속된다. pod 고유 식별인 cgroup 힌트는
// 그대로 학습되어야 한다.
func TestRememberRuntimeHints_HostNetworkSkipsIfindexHint(t *testing.T) {
	e := &Enricher{
		runtimeByCgroup:   make(map[uint64]runtimeCacheEntry),
		runtimeByIfindex:  make(map[uint32]runtimeCacheEntry),
		runtimeTTL:        time.Minute,
		runtimeSweepEvery: time.Minute,
	}

	hn := kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     "kube-system",
		PodName:       "node-exporter-x",
		PodUID:        "u-hn",
		HostNetwork:   true,
	}
	now := time.Now()

	// egress: hostNetwork pod 의 send path 는 물리 NIC 의 __dev_queue_xmit 을 지난다.
	e.rememberRuntimeHints(types.Event{Stage: types.StageToDevQ, CgroupID: 0xaa, Ifindex: 2}, hn, kube.PodIdentity{}, now)
	if _, ok := e.runtimeByCgroup[0xaa]; !ok {
		t.Error("hostNetwork pod 의 cgroup 힌트가 학습되지 않음 (pod 고유 식별이라 학습되어야 함)")
	}
	if _, ok := e.runtimeByIfindex[2]; ok {
		t.Error("hostNetwork pod 의 물리 NIC ifindex 가 힌트로 학습됨 (host 트래픽 오귀속 위험)")
	}

	// ingress: 수신 pod (dst) 의 skb_iif 도 동일하게 차단되어야 한다.
	e.rememberRuntimeHints(types.Event{Stage: types.StageRcvDemux, CgroupID: 0xbb, SkbIif: 2}, kube.PodIdentity{}, hn, now)
	if _, ok := e.runtimeByIfindex[2]; ok {
		t.Error("hostNetwork pod 의 skb_iif 가 힌트로 학습됨")
	}
}

// TestApplyRuntimeHints_NodeSrcReattributedViaScanner 는 #321 의 수용 기준이다. node IP 로 분류된
// src 가 cgroup 역매핑 테이블 (#228 스캐너) 로 pod 에 재귀속되고, kubepods 밖 cgroup (kubelet 등
// host 프로세스) 은 테이블 미스로 기존 host 분류를 유지한다.
func TestApplyRuntimeHints_NodeSrcReattributedViaScanner(t *testing.T) {
	e := &Enricher{
		runtimeByCgroup:   make(map[uint64]runtimeCacheEntry),
		runtimeByIfindex:  make(map[uint32]runtimeCacheEntry),
		runtimeTTL:        time.Minute,
		runtimeSweepEvery: time.Minute,
	}
	sc := NewCgroupScanner(nil, "node-a", "/nonexistent")
	table := map[uint64]kube.PodIdentity{
		0xc1: {IdentityClass: kube.IdentityClassPod, Namespace: "kube-system", PodName: "cilium-x", PodUID: "u-c", NodeName: "node-a", HostNetwork: true},
	}
	sc.table.Store(&table)
	e.SetCgroupScanner(sc)

	nodeSrc := kube.PodIdentity{IdentityClass: kube.IdentityClassNode, NodeName: "node-a", PodIP: "192.168.1.10"}
	now := time.Now()

	src, _ := e.applyRuntimeHints(types.Event{Stage: types.StageSendmsgRet, CgroupID: 0xc1}, "192.168.1.10", "10.0.0.9", nodeSrc, kube.PodIdentity{}, now)
	if !src.IsPod() || src.PodName != "cilium-x" {
		t.Fatalf("node 분류 src 가 스캐너 폴백으로 재귀속되지 않음: %+v", src)
	}
	if src.PodIP != "192.168.1.10" {
		t.Errorf("재귀속된 src 의 관측 IP 미보강: %q", src.PodIP)
	}

	// kubepods 밖 host 프로세스: 테이블 미스로 host 분류 유지.
	src, _ = e.applyRuntimeHints(types.Event{Stage: types.StageSendmsgRet, CgroupID: 0xdd}, "192.168.1.10", "10.0.0.9", nodeSrc, kube.PodIdentity{}, now)
	if !src.IsNode() {
		t.Errorf("host 프로세스 (테이블 미스) 의 src 가 host 분류를 유지하지 않음: %+v", src)
	}
}

const hintTestHostCgroup = 0xbeef

// newHintTestEnricher는 pod 하나(cgroup 0xa1)를 아는 스캐너를 붙인 Enricher와 그 pod를 돌려준다.
// withScanner가 false면 스캐너 없이(cgroup v1 구성) 만든다.
func newHintTestEnricher(withScanner bool) (*Enricher, kube.PodIdentity) {
	e := &Enricher{
		runtimeByCgroup:   make(map[uint64]runtimeCacheEntry),
		runtimeByIfindex:  make(map[uint32]runtimeCacheEntry),
		runtimeTTL:        time.Minute,
		runtimeSweepEvery: time.Minute,
	}
	pod := kube.PodIdentity{IdentityClass: kube.IdentityClassPod, Namespace: "monitoring", PodName: "grafana", PodUID: "u-g", NodeName: "w1"}
	if withScanner {
		sc := NewCgroupScanner(nil, "w1", "/nonexistent")
		table := map[uint64]kube.PodIdentity{0xa1: pod}
		sc.table.Store(&table)
		e.SetCgroupScanner(sc)
	}
	return e, pod
}

// hintStep은 enrich가 하는 대로 힌트 적용과 학습을 차례로 수행한다.
func hintStep(e *Enricher, ev types.Event, src, dst kube.PodIdentity, now time.Time) (kube.PodIdentity, kube.PodIdentity) {
	src, dst = e.applyRuntimeHints(ev, "", "", src, dst, now)
	e.rememberRuntimeHints(ev, src, dst, now)
	return src, dst
}

// TestRuntimeHints_LoopbackIfindexNotShared는 pod가 lo로 보낸 이벤트 뒤에 호스트 프로세스가 lo로 보낸
// 이벤트가 그 pod로 귀속되지 않는지 검증한다. lo는 모든 netns에서 ifindex 1이라, 힌트로 학습되면
// 호스트의 loopback 트래픽이 pod가 되고 이어서 호스트 cgroup 힌트까지 학습된다. 스캐너가 없는
// 구성에서도 같아야 한다.
func TestRuntimeHints_LoopbackIfindexNotShared(t *testing.T) {
	for _, withScanner := range []bool{true, false} {
		e, pod := newHintTestEnricher(withScanner)
		now := time.Now()
		unresolved := kube.PodIdentity{IdentityClass: kube.IdentityClassUnresolved}

		hintStep(e, types.Event{Stage: types.StageToDevQ, CgroupID: 0xa1, Ifindex: 1}, pod, unresolved, now)
		if _, ok := e.runtimeByIfindex[1]; ok {
			t.Errorf("scanner=%v: loopback ifindex가 힌트로 학습됨", withScanner)
		}
		src, _ := hintStep(e, types.Event{Stage: types.StageToDevQ, CgroupID: hintTestHostCgroup, Ifindex: 1}, unresolved, unresolved, now)
		if src.IsPod() {
			t.Errorf("scanner=%v: 호스트의 loopback 이벤트가 pod %s로 귀속됨", withScanner, src.PodName)
		}
		// 수신 방향의 inbound device가 lo인 경우도 같다.
		_, dst := hintStep(e, types.Event{Stage: types.StageRcvDemux, CgroupID: hintTestHostCgroup, SkbIif: 1}, unresolved, unresolved, now)
		if dst.IsPod() {
			t.Errorf("scanner=%v: 호스트의 loopback 수신 이벤트가 pod %s로 귀속됨", withScanner, dst.PodName)
		}
		if _, ok := e.ResolveCgroup(hintTestHostCgroup); ok {
			t.Errorf("scanner=%v: 호스트 cgroup이 pod로 풀림", withScanner)
		}
	}
}

// TestRuntimeHints_ScannerDisablesHints는 스캐너가 있으면 cgroup·ifindex 힌트를 학습하지도 쓰지도
// 않는지 검증한다. 호스트 netns의 인터페이스 번호(eth0의 ifindex 2)가 pod로 학습돼 있어도 스캐너가
// 모르는 cgroup의 이벤트, 곧 호스트 프로세스의 이벤트는 pod가 되지 않아야 한다.
func TestRuntimeHints_ScannerDisablesHints(t *testing.T) {
	e, pod := newHintTestEnricher(true)
	now := time.Now()
	node := kube.PodIdentity{IdentityClass: kube.IdentityClassNode, NodeName: "w1"}
	unresolved := kube.PodIdentity{IdentityClass: kube.IdentityClassUnresolved}

	hintStep(e, types.Event{Stage: types.StageToDevQ, CgroupID: 0xa1, Ifindex: 2, SkbIif: 2}, pod, pod, now)
	if len(e.runtimeByCgroup) != 0 || len(e.runtimeByIfindex) != 0 {
		t.Errorf("스캐너가 있는데 힌트를 학습함: cgroup=%d ifindex=%d", len(e.runtimeByCgroup), len(e.runtimeByIfindex))
	}

	e.rememberIfindexHint(2, pod, now)
	for _, stage := range []uint8{types.StageToDevQ, types.StageRetrans} {
		for _, cgroupID := range []uint64{hintTestHostCgroup, 0} {
			src, _ := hintStep(e, types.Event{Stage: stage, CgroupID: cgroupID, Ifindex: 2}, node, kube.PodIdentity{}, now)
			if src.IsPod() {
				t.Errorf("stage=%d cgroup=%#x: 이벤트가 ifindex 힌트로 pod %s가 됨", stage, cgroupID, src.PodName)
			}
		}
	}
	for _, ev := range []types.Event{
		{Stage: types.StageRcvDemux, CgroupID: hintTestHostCgroup, Ifindex: 2},
		{Stage: types.StageRcvDemux, CgroupID: hintTestHostCgroup, SkbIif: 2},
	} {
		_, dst := hintStep(e, ev, unresolved, node, now)
		if dst.IsPod() {
			t.Errorf("ifindex=%d skb_iif=%d: 호스트 cgroup의 수신 이벤트가 pod %s가 됨", ev.Ifindex, ev.SkbIif, dst.PodName)
		}
	}
}

// TestApplyRuntimeHints_NoScannerKeepsIfindexHint는 스캐너가 없는 구성(cgroup v1)에서 cgroup_id가 있는
// 이벤트도 종전대로 ifindex 힌트를 쓰는지 검증한다.
func TestApplyRuntimeHints_NoScannerKeepsIfindexHint(t *testing.T) {
	e, pod := newHintTestEnricher(false)
	now := time.Now()
	e.rememberIfindexHint(42, pod, now)

	src, _ := e.applyRuntimeHints(types.Event{Stage: types.StageToDevQ, CgroupID: 0x99, Ifindex: 42}, "", "", kube.PodIdentity{}, kube.PodIdentity{}, now)
	if !src.IsPod() {
		t.Errorf("스캐너가 없는데 ifindex 힌트가 안 쓰임: %+v", src)
	}
}
