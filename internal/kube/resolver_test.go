package kube

import "testing"

// newSeededResolver는 informer 없이 IP 인덱스만 직접 채운 Resolver를 만든다.
// 실제 informer 경로는 client-go에 위임된 영역이라 여기서는 ResolveIP의 분기와
// classifyFallbackIP 동작만 격리해 검증한다.
func newSeededResolver() *Resolver {
	return &Resolver{
		podByIP:         make(map[string]podCacheEntry),
		podIPsByKey:     make(map[string][]string),
		serviceByIP:     make(map[string]serviceCacheEntry),
		serviceIPsByKey: make(map[string][]string),
		nodeByIP:        make(map[string]string),
		nodeIPsByKey:    make(map[string][]string),
	}
}

func TestResolveIP_PodHit(t *testing.T) {
	r := newSeededResolver()
	want := PodIdentity{IdentityClass: IdentityClassPod, Namespace: "default", PodName: "p1", PodIP: "10.0.0.1"}
	r.podByIP["10.0.0.1"] = podCacheEntry{key: "p1", id: want}

	got := r.ResolveIP("10.0.0.1")
	if got != want {
		t.Fatalf("ResolveIP=%+v want %+v", got, want)
	}
}

func TestResolveIP_ServiceHit(t *testing.T) {
	r := newSeededResolver()
	want := PodIdentity{IdentityClass: IdentityClassService, Namespace: "default", Workload: "kubernetes", PodIP: "10.96.0.1"}
	r.serviceByIP["10.96.0.1"] = serviceCacheEntry{key: "kubernetes", id: want}

	got := r.ResolveIP("10.96.0.1")
	if got != want {
		t.Fatalf("ResolveIP=%+v want %+v", got, want)
	}
}

func TestResolveIP_NodeHit(t *testing.T) {
	r := newSeededResolver()
	r.nodeByIP["192.168.1.10"] = "node-a"

	got := r.ResolveIP("192.168.1.10")
	if !got.IsNode() {
		t.Fatalf("expected node identity, got class=%q", got.IdentityClass)
	}
	if got.NodeName != "node-a" || got.PodIP != "192.168.1.10" {
		t.Fatalf("ResolveIP=%+v want node-a/192.168.1.10", got)
	}
}

func TestResolveIP_PodTakesPrecedenceOverService(t *testing.T) {
	// 동일 IP에 pod와 service가 모두 등록되어 있으면 pod가 우선이어야 한다.
	// IP 충돌은 비정상이지만 ResolveIP의 lookup 순서가 의도대로 유지되는지를 본다.
	r := newSeededResolver()
	r.podByIP["10.0.0.5"] = podCacheEntry{key: "p", id: PodIdentity{IdentityClass: IdentityClassPod, PodIP: "10.0.0.5"}}
	r.serviceByIP["10.0.0.5"] = serviceCacheEntry{key: "svc", id: PodIdentity{IdentityClass: IdentityClassService, PodIP: "10.0.0.5"}}

	got := r.ResolveIP("10.0.0.5")
	if !got.IsPod() {
		t.Fatalf("expected pod precedence; got class=%q", got.IdentityClass)
	}
}

func TestResolveIP_FallbackClassification(t *testing.T) {
	r := newSeededResolver()
	cases := []struct {
		name      string
		ip        string
		wantClass string
	}{
		{"empty-string", "", IdentityClassUnresolved},
		{"invalid-ip", "not-an-ip", IdentityClassUnresolved},
		{"loopback", "127.0.0.1", IdentityClassUnresolved},
		{"unspecified", "0.0.0.0", IdentityClassUnresolved},
		{"link-local", "169.254.1.1", IdentityClassUnresolved},
		{"multicast", "224.0.0.1", IdentityClassUnresolved},
		{"rfc1918-private-10", "10.99.0.1", IdentityClassUnresolved},
		{"rfc1918-private-172", "172.20.0.1", IdentityClassUnresolved},
		{"rfc1918-private-192", "192.168.99.1", IdentityClassUnresolved},
		{"public-ipv4", "8.8.8.8", IdentityClassExternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.ResolveIP(tc.ip)
			if got.IdentityClass != tc.wantClass {
				t.Errorf("ResolveIP(%q) class=%q want %q", tc.ip, got.IdentityClass, tc.wantClass)
			}
			if tc.ip != "" && got.PodIP != tc.ip {
				t.Errorf("ResolveIP(%q) PodIP=%q want %q", tc.ip, got.PodIP, tc.ip)
			}
		})
	}
}

func TestStrongerIdentity_RankWins(t *testing.T) {
	weaker := PodIdentity{IdentityClass: IdentityClassUnresolved}
	stronger := PodIdentity{IdentityClass: IdentityClassPod}

	if got := StrongerIdentity(weaker, stronger); !got.IsPod() {
		t.Fatalf("stronger candidate should win; got class=%q", got.IdentityClass)
	}
	if got := StrongerIdentity(stronger, weaker); !got.IsPod() {
		t.Fatalf("stronger current should be kept; got class=%q", got.IdentityClass)
	}
}

func TestStrongerIdentity_CompletenessTiebreak(t *testing.T) {
	// 같은 IdentityClass(Pod)일 때 채워진 필드가 더 많은 쪽이 이긴다.
	sparse := PodIdentity{IdentityClass: IdentityClassPod, PodIP: "10.0.0.1"}
	rich := PodIdentity{IdentityClass: IdentityClassPod, Namespace: "default", PodName: "p1", PodUID: "uid1", PodIP: "10.0.0.1"}

	got := StrongerIdentity(sparse, rich)
	if got.PodName != "p1" {
		t.Fatalf("richer candidate should win; got %+v", got)
	}
}

func TestWithObservedIP(t *testing.T) {
	id := PodIdentity{IdentityClass: IdentityClassPod, PodName: "p1"}

	if got := WithObservedIP(id, "10.0.0.1"); got.PodIP != "10.0.0.1" {
		t.Errorf("WithObservedIP should set PodIP; got %q", got.PodIP)
	}
	if got := WithObservedIP(id, ""); got.PodIP != "" {
		t.Errorf("empty IP should not overwrite (here both empty); got %q", got.PodIP)
	}

	// 이미 PodIP가 채워진 식별에 빈 IP를 넘기면 그대로 보존되어야 한다.
	idWithIP := PodIdentity{IdentityClass: IdentityClassPod, PodIP: "10.0.0.5"}
	if got := WithObservedIP(idWithIP, ""); got.PodIP != "10.0.0.5" {
		t.Errorf("empty IP must not clear existing PodIP; got %q", got.PodIP)
	}
}

func TestNewResolver_NoConfigStillReturnsResolver(t *testing.T) {
	// in-cluster 또는 kubeconfig 둘 다 없는 환경에서도 NewResolver가 nil을 반환하지 않고
	// 비활성 Resolver를 반환해야 한다. Start 시 disabled 로그 후 반환되는 graceful path 전제다.
	t.Setenv("KUBECONFIG", "/nonexistent/path")
	t.Setenv("HOME", "/nonexistent-home")

	r := NewResolver("test-node", 0)
	if r == nil {
		t.Fatal("NewResolver returned nil")
	}
	if r.LocalNode() != "test-node" {
		t.Errorf("LocalNode=%q want test-node", r.LocalNode())
	}
	if r.HasSynced() {
		t.Error("HasSynced should be false on a never-started resolver")
	}
}
