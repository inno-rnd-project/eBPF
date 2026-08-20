package metadata

import (
	"testing"
	"time"

	"netobs/internal/types"
)

// newTestResolver는 K8s 클라이언트 없이 내부 로직만 테스트하기 위한
// 최소 초기화된 Resolver를 반환한다.
func newTestResolver() *Resolver {
	return &Resolver{
		localNode:       "test-node",
		podByIP:         make(map[string]podCacheEntry),
		podIPsByKey:     make(map[string][]string),
		serviceByIP:     make(map[string]serviceCacheEntry),
		serviceIPsByKey: make(map[string][]string),
		nodeByIP:        make(map[string]string),
		nodeIPsByKey:    make(map[string][]string),

		flowCurrent:     make(map[uint64]flowCacheEntry),
		flowPrevious:    make(map[uint64]flowCacheEntry),
		flowRotateEvery: 2*time.Minute + 30*time.Second,
		flowMaxCurrent:  100_000,
		lastFlowRotate:  time.Now(),

		runtimeByCgroup:   make(map[uint64]runtimeCacheEntry),
		runtimeByIfindex:  make(map[uint32]runtimeCacheEntry),
		runtimeTTL:        2 * time.Minute,
		runtimeSweepEvery: 30 * time.Second,
	}
}

// -------------------------------------------------------------------
// identityCompleteness
// -------------------------------------------------------------------

func TestIdentityCompleteness(t *testing.T) {
	cases := []struct {
		name  string
		id    types.PodIdentity
		score int
	}{
		{
			name:  "빈 identity",
			id:    types.PodIdentity{},
			score: 0,
		},
		{
			name: "필드 하나",
			id:   types.PodIdentity{Namespace: "ns"},
			score: 1,
		},
		{
			name: "모든 필드 채움 (7점)",
			id: types.PodIdentity{
				Namespace:    "ns",
				PodUID:       "uid",
				PodName:      "pod",
				NodeName:     "node",
				Workload:     "deploy",
				WorkloadKind: "Deployment",
				PodIP:        "10.0.0.1",
			},
			score: 7,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := identityCompleteness(c.id)
			if got != c.score {
				t.Errorf("identityCompleteness() = %d, want %d", got, c.score)
			}
		})
	}
}

// -------------------------------------------------------------------
// strongerIdentity
// -------------------------------------------------------------------

func TestStrongerIdentity(t *testing.T) {
	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "a"}
	ext := types.PodIdentity{IdentityClass: types.IdentityClassExternal, PodIP: "8.8.8.8"}

	t.Run("higher rank wins", func(t *testing.T) {
		got := strongerIdentity(ext, pod)
		if got.IdentityClass != types.IdentityClassPod {
			t.Errorf("expected pod identity, got %q", got.IdentityClass)
		}
	})

	t.Run("current wins when candidate rank is lower", func(t *testing.T) {
		got := strongerIdentity(pod, ext)
		if got.IdentityClass != types.IdentityClassPod {
			t.Errorf("expected pod identity, got %q", got.IdentityClass)
		}
	})

	t.Run("same rank, more complete candidate wins", func(t *testing.T) {
		sparse := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "sparse"}
		rich := types.PodIdentity{
			IdentityClass: types.IdentityClassPod,
			PodName:       "rich",
			Namespace:     "ns",
			PodUID:        "uid",
		}
		got := strongerIdentity(sparse, rich)
		if got.PodName != "rich" {
			t.Errorf("expected richer identity to win, got PodName=%q", got.PodName)
		}
	})

	t.Run("same rank, same completeness → current wins", func(t *testing.T) {
		a := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "a-pod"}
		b := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "b-pod"}
		got := strongerIdentity(a, b)
		if got.PodName != "a-pod" {
			t.Errorf("expected current (a-pod) to win on tie, got %q", got.PodName)
		}
	})
}

// -------------------------------------------------------------------
// deriveTrafficScope
// -------------------------------------------------------------------

func TestDeriveTrafficScope(t *testing.T) {
	pod := func(node string) types.PodIdentity {
		return types.PodIdentity{IdentityClass: types.IdentityClassPod, NodeName: node}
	}
	svc := types.PodIdentity{IdentityClass: types.IdentityClassService}
	ext := types.PodIdentity{IdentityClass: types.IdentityClassExternal}
	nodeID := func(name string) types.PodIdentity {
		return types.PodIdentity{IdentityClass: types.IdentityClassNode, NodeName: name}
	}
	unresolved := types.PodIdentity{IdentityClass: types.IdentityClassUnresolved}

	cases := []struct {
		name string
		src  types.PodIdentity
		dst  types.PodIdentity
		want string
	}{
		{"pod-to-pod same node", pod("n1"), pod("n1"), "same_node"},
		{"pod-to-pod cross node", pod("n1"), pod("n2"), "cross_node"},
		{"pod-to-pod no node info", pod(""), pod(""), "pod_to_pod"},
		{"pod-to-service", pod("n1"), svc, "to_service"},
		{"service-to-pod", svc, pod("n1"), "from_service"},
		{"pod-to-external", pod("n1"), ext, "to_external"},
		{"external-to-pod", ext, pod("n1"), "from_external"},
		{"pod-to-node host-local", pod("n1"), nodeID("n1"), "to_host_local"},
		{"pod-to-node cross", pod("n1"), nodeID("n2"), "to_node"},
		{"node-to-pod host-local", nodeID("n1"), pod("n1"), "from_host_local"},
		{"node-to-pod cross", nodeID("n1"), pod("n2"), "from_node"},
		{"node-to-node same", nodeID("n1"), nodeID("n1"), "host_local"},
		{"node-to-node diff", nodeID("n1"), nodeID("n2"), "node_to_node"},
		{"service-to-external", svc, ext, "service_to_external"},
		{"external-to-service", ext, svc, "external_to_service"},
		{"pod-to-unresolved", pod("n1"), unresolved, "to_unresolved"},
		{"unresolved-to-pod", unresolved, pod("n1"), "from_unresolved"},
		{"service-to-unresolved", svc, unresolved, "service_to_unresolved"},
		{"unresolved-to-service", unresolved, svc, "unresolved_to_service"},
		{"default (ext-to-ext)", ext, ext, "unresolved"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveTrafficScope(c.src, c.dst)
			if got != c.want {
				t.Errorf("deriveTrafficScope() = %q, want %q", got, c.want)
			}
		})
	}
}

// -------------------------------------------------------------------
// classifyFallbackIP
// -------------------------------------------------------------------

func TestClassifyFallbackIP(t *testing.T) {
	cases := []struct {
		ip        string
		wantClass string
	}{
		// 루프백 → unresolved
		{"127.0.0.1", types.IdentityClassUnresolved},
		{"::1", types.IdentityClassUnresolved},
		// RFC1918 (private) → unresolved
		{"192.168.1.1", types.IdentityClassUnresolved},
		{"10.0.0.1", types.IdentityClassUnresolved},
		{"172.16.0.1", types.IdentityClassUnresolved},
		// 멀티캐스트 → unresolved
		{"224.0.0.1", types.IdentityClassUnresolved},
		{"239.255.255.255", types.IdentityClassUnresolved},
		// 링크로컬 → unresolved
		{"169.254.1.1", types.IdentityClassUnresolved},
		{"fe80::1", types.IdentityClassUnresolved},
		// 비지정 → unresolved
		{"0.0.0.0", types.IdentityClassUnresolved},
		// public IP → external
		{"8.8.8.8", types.IdentityClassExternal},
		{"1.1.1.1", types.IdentityClassExternal},
		// 잘못된 IP 문자열 → unresolved
		{"not-an-ip", types.IdentityClassUnresolved},
		{"", types.IdentityClassUnresolved},
	}

	for _, c := range cases {
		t.Run(c.ip, func(t *testing.T) {
			got := classifyFallbackIP(c.ip)
			if got.IdentityClass != c.wantClass {
				t.Errorf("classifyFallbackIP(%q).IdentityClass = %q, want %q",
					c.ip, got.IdentityClass, c.wantClass)
			}
			// 입력 IP 문자열은 어떤 경우든 PodIP에 그대로 기록되어야 한다.
			if c.ip != "" && got.PodIP != c.ip {
				t.Errorf("classifyFallbackIP(%q).PodIP = %q, want %q",
					c.ip, got.PodIP, c.ip)
			}
		})
	}
}

// -------------------------------------------------------------------
// Flow cache: rememberFlow + lookupFlow (current hit)
// -------------------------------------------------------------------

func TestFlowCacheBasic(t *testing.T) {
	r := newTestResolver()
	now := time.Now()

	src := types.PodIdentity{
		IdentityClass: types.IdentityClassPod,
		PodName:       "src-pod",
	}
	dst := types.PodIdentity{
		IdentityClass: types.IdentityClassService,
		Workload:      "my-svc",
	}
	cookie := uint64(12345)

	r.rememberFlow(cookie, src, dst, now)

	entry, ok := r.lookupFlow(cookie)
	if !ok {
		t.Fatal("lookupFlow: current hit 기대, 결과 없음")
	}
	if entry.Src.PodName != "src-pod" {
		t.Errorf("entry.Src.PodName = %q, want %q", entry.Src.PodName, "src-pod")
	}
	if entry.Dst.Workload != "my-svc" {
		t.Errorf("entry.Dst.Workload = %q, want %q", entry.Dst.Workload, "my-svc")
	}
}

func TestFlowCacheMiss(t *testing.T) {
	r := newTestResolver()
	_, ok := r.lookupFlow(99999)
	if ok {
		t.Error("lookupFlow: 없는 cookie에 hit 반환 안 돼야 함")
	}
}

func TestFlowCacheCookieZero(t *testing.T) {
	r := newTestResolver()
	src := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "p"}

	// cookie=0 는 무시돼야 한다.
	r.rememberFlow(0, src, types.PodIdentity{}, time.Now())
	r.mu.RLock()
	n := len(r.flowCurrent)
	r.mu.RUnlock()
	if n != 0 {
		t.Errorf("cookie=0 은 저장되면 안 됨, flowCurrent len=%d", n)
	}

	// lookupFlow(0) 도 false를 반환해야 한다.
	_, ok := r.lookupFlow(0)
	if ok {
		t.Error("lookupFlow(0) 는 false 반환해야 함")
	}
}

func TestFlowCacheUnresolvedSrcNotStored(t *testing.T) {
	r := newTestResolver()
	unresolved := types.PodIdentity{IdentityClass: types.IdentityClassUnresolved}

	r.rememberFlow(111, unresolved, types.PodIdentity{}, time.Now())
	r.mu.RLock()
	n := len(r.flowCurrent)
	r.mu.RUnlock()
	if n != 0 {
		t.Errorf("Known()=false인 src 는 저장되면 안 됨, flowCurrent len=%d", n)
	}
}

// -------------------------------------------------------------------
// Flow cache: maybeRotateFlowsLocked – 시간 기반 rotate
// -------------------------------------------------------------------

func TestFlowCacheTimeRotation(t *testing.T) {
	r := newTestResolver()
	r.flowRotateEvery = 100 * time.Millisecond

	now := time.Now()
	src := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "p1"}
	cookie := uint64(42)

	r.rememberFlow(cookie, src, types.PodIdentity{}, now)

	// 아직 rotate 전 – current에 있어야 한다.
	r.mu.RLock()
	_, inCurrent := r.flowCurrent[cookie]
	r.mu.RUnlock()
	if !inCurrent {
		t.Fatal("rotate 전: cookie는 flowCurrent에 있어야 함")
	}

	// 시간 기반 rotate 트리거
	futureNow := now.Add(200 * time.Millisecond)
	r.mu.Lock()
	r.maybeRotateFlowsLocked(futureNow)
	r.mu.Unlock()

	// current → previous 이동 확인
	r.mu.RLock()
	_, inCurrent = r.flowCurrent[cookie]
	_, inPrevious := r.flowPrevious[cookie]
	r.mu.RUnlock()

	if inCurrent {
		t.Error("rotate 후: cookie는 flowCurrent에 없어야 함")
	}
	if !inPrevious {
		t.Error("rotate 후: cookie는 flowPrevious에 있어야 함")
	}

	// lookupFlow 가 previous에서 찾아 current로 promote 해야 한다.
	entry, ok := r.lookupFlow(cookie)
	if !ok {
		t.Fatal("lookupFlow: previous hit 기대, 결과 없음")
	}
	if entry.Src.PodName != "p1" {
		t.Errorf("entry.Src.PodName = %q, want %q", entry.Src.PodName, "p1")
	}

	// promote 확인 – current에 다시 들어와야 한다.
	r.mu.RLock()
	_, promotedToCurrent := r.flowCurrent[cookie]
	r.mu.RUnlock()
	if !promotedToCurrent {
		t.Error("lookupFlow previous hit: entry가 flowCurrent로 promote 되지 않음")
	}
}

// -------------------------------------------------------------------
// Flow cache: maybeRotateFlowsLocked – 크기 기반 rotate
// -------------------------------------------------------------------

func TestFlowCacheSizeRotation(t *testing.T) {
	r := newTestResolver()
	r.flowMaxCurrent = 2 // 2개 이상이면 다음 insert 시 rotate

	now := time.Now()
	src := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "p"}

	// 2개 추가 (rotate 미발생)
	r.rememberFlow(1, src, types.PodIdentity{}, now)
	r.rememberFlow(2, src, types.PodIdentity{}, now)

	r.mu.RLock()
	curLen := len(r.flowCurrent)
	r.mu.RUnlock()
	if curLen != 2 {
		t.Fatalf("2개 추가 후 flowCurrent len=%d, want 2", curLen)
	}

	// 3번째 추가 – len>=2 이므로 먼저 rotate, 이후 cookie=3 삽입
	r.rememberFlow(3, src, types.PodIdentity{}, now)

	r.mu.RLock()
	_, c1 := r.flowCurrent[1]
	_, c2 := r.flowCurrent[2]
	_, c3 := r.flowCurrent[3]
	_, p1 := r.flowPrevious[1]
	_, p2 := r.flowPrevious[2]
	r.mu.RUnlock()

	if c1 || c2 {
		t.Error("크기 기반 rotate 후: cookie 1,2는 flowCurrent에 없어야 함")
	}
	if !c3 {
		t.Error("rotate 후 신규 삽입된 cookie 3은 flowCurrent에 있어야 함")
	}
	if !p1 || !p2 {
		t.Error("크기 기반 rotate 후: cookie 1,2는 flowPrevious에 있어야 함")
	}
}

// -------------------------------------------------------------------
// Runtime hint: TTL 확인 (cgroup)
// -------------------------------------------------------------------

func TestRuntimeHintCgroupTTL(t *testing.T) {
	r := newTestResolver()
	r.runtimeTTL = 5 * time.Minute

	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "p1"}
	now := time.Now()
	cgroupID := uint64(999)

	r.rememberCgroupHint(cgroupID, pod, now)

	// TTL 이내 → 조회 성공
	id, ok := r.lookupCgroupHint(cgroupID, now.Add(1*time.Minute))
	if !ok {
		t.Fatal("TTL 이내에서 cgroup hint를 찾지 못함")
	}
	if id.PodName != "p1" {
		t.Errorf("PodName = %q, want %q", id.PodName, "p1")
	}

	// TTL 초과 → 조회 실패
	_, ok = r.lookupCgroupHint(cgroupID, now.Add(6*time.Minute))
	if ok {
		t.Error("TTL 초과 후에도 cgroup hint가 반환됨")
	}
}

// -------------------------------------------------------------------
// Runtime hint: TTL 확인 (ifindex)
// -------------------------------------------------------------------

func TestRuntimeHintIfindexTTL(t *testing.T) {
	r := newTestResolver()
	r.runtimeTTL = 3 * time.Minute

	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "p2"}
	now := time.Now()
	ifindex := uint32(5)

	r.rememberIfindexHint(ifindex, pod, now)

	// TTL 이내 → 조회 성공
	id, ok := r.lookupIfindexHint(ifindex, now.Add(2*time.Minute))
	if !ok {
		t.Fatal("TTL 이내에서 ifindex hint를 찾지 못함")
	}
	if id.PodName != "p2" {
		t.Errorf("PodName = %q, want %q", id.PodName, "p2")
	}

	// TTL 초과 → 조회 실패
	_, ok = r.lookupIfindexHint(ifindex, now.Add(4*time.Minute))
	if ok {
		t.Error("TTL 초과 후에도 ifindex hint가 반환됨")
	}
}

// -------------------------------------------------------------------
// Runtime hint: 0값 입력 무시 (cgroup/ifindex == 0, non-pod)
// -------------------------------------------------------------------

func TestRuntimeHintIgnoreZeroOrNonPod(t *testing.T) {
	r := newTestResolver()
	now := time.Now()

	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "p"}
	ext := types.PodIdentity{IdentityClass: types.IdentityClassExternal}

	// cgroupID=0 → 무시
	r.rememberCgroupHint(0, pod, now)
	r.mu.RLock()
	if len(r.runtimeByCgroup) != 0 {
		t.Errorf("cgroupID=0은 저장되면 안 됨, len=%d", len(r.runtimeByCgroup))
	}
	r.mu.RUnlock()

	// non-pod identity → 무시
	r.rememberCgroupHint(1, ext, now)
	r.mu.RLock()
	if len(r.runtimeByCgroup) != 0 {
		t.Errorf("non-pod identity는 cgroup hint로 저장되면 안 됨, len=%d", len(r.runtimeByCgroup))
	}
	r.mu.RUnlock()

	// ifindex=0 → 무시
	r.rememberIfindexHint(0, pod, now)
	r.mu.RLock()
	if len(r.runtimeByIfindex) != 0 {
		t.Errorf("ifindex=0은 저장되면 안 됨, len=%d", len(r.runtimeByIfindex))
	}
	r.mu.RUnlock()
}

// -------------------------------------------------------------------
// maybeSweepRuntimeLocked: 시간 기반 sweep 동작
// -------------------------------------------------------------------

func TestMaybeSweepRuntimeLocked(t *testing.T) {
	r := newTestResolver()
	r.runtimeTTL = 1 * time.Minute
	r.runtimeSweepEvery = 30 * time.Second

	now := time.Now()
	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "p1"}

	// 내부 맵에 직접 엔트리 삽입 (sweep 자체를 독립적으로 테스트하기 위함)
	r.mu.Lock()
	r.runtimeByCgroup[100] = runtimeCacheEntry{ID: pod, LastSeen: now}
	r.runtimeByIfindex[10] = runtimeCacheEntry{ID: pod, LastSeen: now}
	r.lastRuntimeSweep = now // sweep 간격 기준점 초기화
	r.mu.Unlock()

	// sweep 간격 이내 → sweep 스킵, 엔트리 유지
	r.mu.Lock()
	r.maybeSweepRuntimeLocked(now.Add(10 * time.Second))
	r.mu.Unlock()

	r.mu.RLock()
	cLen := len(r.runtimeByCgroup)
	iLen := len(r.runtimeByIfindex)
	r.mu.RUnlock()
	if cLen != 1 || iLen != 1 {
		t.Errorf("이른 sweep: 엔트리 유지 기대 (cgroup=%d, ifindex=%d)", cLen, iLen)
	}

	// sweep 간격 경과 + TTL 초과 → sweep 수행, 엔트리 제거
	expiredNow := now.Add(2 * time.Minute)
	r.mu.Lock()
	r.maybeSweepRuntimeLocked(expiredNow)
	r.mu.Unlock()

	r.mu.RLock()
	cLen = len(r.runtimeByCgroup)
	iLen = len(r.runtimeByIfindex)
	r.mu.RUnlock()
	if cLen != 0 || iLen != 0 {
		t.Errorf("TTL 초과 sweep: 엔트리 제거 기대 (cgroup=%d, ifindex=%d)", cLen, iLen)
	}
}

// -------------------------------------------------------------------
// applyRuntimeHints: cgroup hint 우선
// -------------------------------------------------------------------

func TestApplyRuntimeHintsCgroup(t *testing.T) {
	r := newTestResolver()
	r.runtimeTTL = 5 * time.Minute

	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "cgroup-pod"}
	now := time.Now()
	cgroupID := uint64(777)

	r.rememberCgroupHint(cgroupID, pod, now)

	ev := types.Event{CgroupID: cgroupID}
	unresolved := types.PodIdentity{IdentityClass: types.IdentityClassUnresolved}

	src, _ := r.applyRuntimeHints(ev, "10.0.0.1", "10.0.0.2", unresolved, unresolved, now)

	if !src.IsPod() {
		t.Fatalf("cgroup hint 적용 후 src는 pod이어야 함, got %q", src.IdentityClass)
	}
	if src.PodName != "cgroup-pod" {
		t.Errorf("src.PodName = %q, want %q", src.PodName, "cgroup-pod")
	}
	// withObservedIP 로 srcIP가 PodIP에 기록되어야 한다.
	if src.PodIP != "10.0.0.1" {
		t.Errorf("src.PodIP = %q, want %q", src.PodIP, "10.0.0.1")
	}
}

// -------------------------------------------------------------------
// applyRuntimeHints: ifindex fallback (cgroup miss 시)
// -------------------------------------------------------------------

func TestApplyRuntimeHintsIfindexFallback(t *testing.T) {
	r := newTestResolver()
	r.runtimeTTL = 5 * time.Minute

	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "ifindex-pod"}
	now := time.Now()
	ifindex := uint32(5)

	r.rememberIfindexHint(ifindex, pod, now)

	// CgroupID는 힌트가 없는 값, Ifindex는 힌트가 있는 값
	ev := types.Event{CgroupID: 9999, Ifindex: ifindex}
	unresolved := types.PodIdentity{IdentityClass: types.IdentityClassUnresolved}

	src, _ := r.applyRuntimeHints(ev, "10.0.0.1", "10.0.0.2", unresolved, unresolved, now)

	if !src.IsPod() {
		t.Fatalf("ifindex fallback 적용 후 src는 pod이어야 함, got %q", src.IdentityClass)
	}
	if src.PodName != "ifindex-pod" {
		t.Errorf("src.PodName = %q, want %q", src.PodName, "ifindex-pod")
	}
}

// -------------------------------------------------------------------
// applyRuntimeHints: dst skbIif hint
// -------------------------------------------------------------------

func TestApplyRuntimeHintsDst(t *testing.T) {
	r := newTestResolver()
	r.runtimeTTL = 5 * time.Minute

	pod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "dst-pod"}
	now := time.Now()
	skbIif := uint32(10)

	r.rememberIfindexHint(skbIif, pod, now)

	ev := types.Event{SkbIif: skbIif}
	unresolved := types.PodIdentity{IdentityClass: types.IdentityClassUnresolved}

	_, dst := r.applyRuntimeHints(ev, "10.0.0.1", "10.0.0.2", unresolved, unresolved, now)

	if !dst.IsPod() {
		t.Fatalf("skbIif hint 적용 후 dst는 pod이어야 함, got %q", dst.IdentityClass)
	}
	if dst.PodName != "dst-pod" {
		t.Errorf("dst.PodName = %q, want %q", dst.PodName, "dst-pod")
	}
}

// -------------------------------------------------------------------
// applyRuntimeHints: cgroup이 pod으로 업그레이드되면 ifindex는 건너뜀
// -------------------------------------------------------------------

func TestApplyRuntimeHintsCgroupPriority(t *testing.T) {
	r := newTestResolver()
	r.runtimeTTL = 5 * time.Minute

	cgroupPod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "cgroup-pod"}
	ifindexPod := types.PodIdentity{IdentityClass: types.IdentityClassPod, PodName: "ifindex-pod"}
	now := time.Now()

	cgroupID := uint64(1)
	ifindex := uint32(2)

	r.rememberCgroupHint(cgroupID, cgroupPod, now)
	r.rememberIfindexHint(ifindex, ifindexPod, now)

	// cgroup과 ifindex 모두 힌트가 있을 때 cgroup이 먼저 적용되고
	// 그 결과 src가 pod이 되면 ifindex는 체크하지 않아야 한다.
	ev := types.Event{CgroupID: cgroupID, Ifindex: ifindex}
	unresolved := types.PodIdentity{IdentityClass: types.IdentityClassUnresolved}

	src, _ := r.applyRuntimeHints(ev, "10.0.0.1", "10.0.0.2", unresolved, unresolved, now)

	if src.PodName != "cgroup-pod" {
		t.Errorf("cgroup 우선: src.PodName = %q, want %q", src.PodName, "cgroup-pod")
	}
}
