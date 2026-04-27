package kube

import "testing"

func TestPodIdentity_ClassPredicates(t *testing.T) {
	cases := []struct {
		name    string
		id      PodIdentity
		isPod   bool
		isSvc   bool
		isNode  bool
		isExt   bool
		isUnres bool
		isKnown bool
	}{
		{"pod", PodIdentity{IdentityClass: IdentityClassPod}, true, false, false, false, false, true},
		{"service", PodIdentity{IdentityClass: IdentityClassService}, false, true, false, false, false, true},
		{"node", PodIdentity{IdentityClass: IdentityClassNode}, false, false, true, false, false, true},
		{"external", PodIdentity{IdentityClass: IdentityClassExternal}, false, false, false, true, false, true},
		{"unresolved-explicit", PodIdentity{IdentityClass: IdentityClassUnresolved}, false, false, false, false, true, false},
		{"unresolved-zero", PodIdentity{}, false, false, false, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.IsPod(); got != tc.isPod {
				t.Errorf("IsPod=%v want %v", got, tc.isPod)
			}
			if got := tc.id.IsService(); got != tc.isSvc {
				t.Errorf("IsService=%v want %v", got, tc.isSvc)
			}
			if got := tc.id.IsNode(); got != tc.isNode {
				t.Errorf("IsNode=%v want %v", got, tc.isNode)
			}
			if got := tc.id.IsExternal(); got != tc.isExt {
				t.Errorf("IsExternal=%v want %v", got, tc.isExt)
			}
			if got := tc.id.IsUnresolved(); got != tc.isUnres {
				t.Errorf("IsUnresolved=%v want %v", got, tc.isUnres)
			}
			if got := tc.id.Known(); got != tc.isKnown {
				t.Errorf("Known=%v want %v", got, tc.isKnown)
			}
		})
	}
}

func TestPodIdentity_NamespaceLabel(t *testing.T) {
	cases := []struct {
		name string
		id   PodIdentity
		want string
	}{
		{"pod-with-ns", PodIdentity{IdentityClass: IdentityClassPod, Namespace: "kube-system"}, "kube-system"},
		{"pod-without-ns", PodIdentity{IdentityClass: IdentityClassPod}, "unknown"},
		{"node-always-host", PodIdentity{IdentityClass: IdentityClassNode}, "host"},
		{"service-with-ns", PodIdentity{IdentityClass: IdentityClassService, Namespace: "default"}, "default"},
		{"service-without-ns", PodIdentity{IdentityClass: IdentityClassService}, "service"},
		{"external", PodIdentity{IdentityClass: IdentityClassExternal}, "external"},
		{"unresolved", PodIdentity{IdentityClass: IdentityClassUnresolved}, "unresolved"},
		{"zero", PodIdentity{}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.NamespaceLabel(); got != tc.want {
				t.Errorf("NamespaceLabel=%q want %q", got, tc.want)
			}
		})
	}
}

func TestPodIdentity_WorkloadLabel(t *testing.T) {
	cases := []struct {
		name string
		id   PodIdentity
		want string
	}{
		{
			name: "pod-deployment-trims-hash",
			id:   PodIdentity{IdentityClass: IdentityClassPod, WorkloadKind: "ReplicaSet", Workload: "frontend-7d4f9b8c5", PodName: "frontend-7d4f9b8c5-xyz12"},
			want: "frontend",
		},
		{
			name: "pod-statefulset-keeps-name",
			id:   PodIdentity{IdentityClass: IdentityClassPod, WorkloadKind: "StatefulSet", Workload: "redis-0"},
			want: "redis-0",
		},
		{
			name: "pod-no-workload-falls-back-to-podname",
			id:   PodIdentity{IdentityClass: IdentityClassPod, PodName: "naked-pod"},
			want: "naked-pod",
		},
		{
			name: "pod-empty-everything",
			id:   PodIdentity{IdentityClass: IdentityClassPod},
			want: "unknown",
		},
		{
			name: "node-with-name",
			id:   PodIdentity{IdentityClass: IdentityClassNode, NodeName: "node-1"},
			want: "node/node-1",
		},
		{
			name: "node-without-name",
			id:   PodIdentity{IdentityClass: IdentityClassNode},
			want: "host-network",
		},
		{
			name: "service-with-name",
			id:   PodIdentity{IdentityClass: IdentityClassService, Workload: "kubernetes"},
			want: "svc/kubernetes",
		},
		{
			name: "external",
			id:   PodIdentity{IdentityClass: IdentityClassExternal},
			want: "external",
		},
		{
			name: "unresolved",
			id:   PodIdentity{IdentityClass: IdentityClassUnresolved},
			want: "unresolved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.WorkloadLabel(); got != tc.want {
				t.Errorf("WorkloadLabel=%q want %q", got, tc.want)
			}
		})
	}
}

func TestPodIdentity_Rank(t *testing.T) {
	// Rank가 단순한 상수 매핑이라 정확한 값보다는 우선순위 관계가 깨지지 않는지를 본다.
	pod := PodIdentity{IdentityClass: IdentityClassPod}
	svc := PodIdentity{IdentityClass: IdentityClassService}
	node := PodIdentity{IdentityClass: IdentityClassNode}
	ext := PodIdentity{IdentityClass: IdentityClassExternal}
	un := PodIdentity{IdentityClass: IdentityClassUnresolved}

	if !(pod.Rank() > svc.Rank() && svc.Rank() > node.Rank() && node.Rank() > ext.Rank() && ext.Rank() > un.Rank()) {
		t.Fatalf("rank ordering broken: pod=%d svc=%d node=%d ext=%d un=%d",
			pod.Rank(), svc.Rank(), node.Rank(), ext.Rank(), un.Rank())
	}
}

func TestPodIdentity_String(t *testing.T) {
	cases := []struct {
		name string
		id   PodIdentity
		want string
	}{
		{"pod-ns-name", PodIdentity{IdentityClass: IdentityClassPod, Namespace: "default", PodName: "p1"}, "default/p1"},
		{"pod-ip-fallback", PodIdentity{IdentityClass: IdentityClassPod, PodIP: "10.0.0.1"}, "10.0.0.1"},
		{"pod-empty", PodIdentity{IdentityClass: IdentityClassPod}, "pod"},
		{"node-name", PodIdentity{IdentityClass: IdentityClassNode, NodeName: "n1"}, "node/n1"},
		{"service-ns-workload", PodIdentity{IdentityClass: IdentityClassService, Namespace: "default", Workload: "svc"}, "default/svc/svc"},
		{"external-ip", PodIdentity{IdentityClass: IdentityClassExternal, PodIP: "1.1.1.1"}, "1.1.1.1"},
		{"unresolved-empty", PodIdentity{IdentityClass: IdentityClassUnresolved}, "unresolved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.String(); got != tc.want {
				t.Errorf("String=%q want %q", got, tc.want)
			}
		})
	}
}

func TestPodIdentity_WorkloadKey(t *testing.T) {
	cases := []struct {
		name string
		id   PodIdentity
		want string
	}{
		{
			name: "pod-with-deployment",
			id:   PodIdentity{IdentityClass: IdentityClassPod, Namespace: "default", WorkloadKind: "Deployment", Workload: "api"},
			want: "default/Deployment/api",
		},
		{
			name: "pod-without-kind",
			id:   PodIdentity{IdentityClass: IdentityClassPod, Namespace: "default", PodName: "naked"},
			want: "default/Pod/naked",
		},
		{
			name: "service",
			id:   PodIdentity{IdentityClass: IdentityClassService, Namespace: "default", Workload: "kubernetes"},
			want: "default/Service/svc/kubernetes",
		},
		{
			name: "node",
			id:   PodIdentity{IdentityClass: IdentityClassNode, NodeName: "n1"},
			want: "host/Node/node/n1",
		},
		{
			name: "external",
			id:   PodIdentity{IdentityClass: IdentityClassExternal},
			want: "external/External/external",
		},
		{
			name: "unresolved",
			id:   PodIdentity{IdentityClass: IdentityClassUnresolved},
			want: "unresolved/Unresolved/unresolved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.WorkloadKey(); got != tc.want {
				t.Errorf("WorkloadKey=%q want %q", got, tc.want)
			}
		})
	}
}

func TestTrimGeneratedSuffix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"deployment-with-rs-hash", "frontend-7d4f9b8c5", "frontend"},
		{"two-level-name-with-hash", "frontend-api-7d4f9b8c5", "frontend-api"},
		{"single-segment", "frontend", ""},
		{"hash-too-short", "frontend-abc", ""},
		{"hash-too-long-still-valid", "frontend-abcdef0123456", "frontend"},
		{"hash-way-too-long", "frontend-abcdef01234567890", ""},
		{"hash-uppercase-not-hashlike", "frontend-ABCDEF01", ""},
		{"hash-with-symbol-not-hashlike", "frontend-abcdef0!", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TrimGeneratedSuffix(tc.in); got != tc.want {
				t.Errorf("TrimGeneratedSuffix(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeWorkloadName(t *testing.T) {
	// hashlike 휴리스틱은 length 8~16의 lowercase 영숫자 suffix면 모두 trim하므로
	// "node-exporter"처럼 8자 영문자 suffix가 붙은 이름은 의도와 무관하게 잘려나간다.
	// 본 테스트는 그런 false-positive를 우회하는 짧은 suffix(5자) 이름으로 통과 동작을 검증한다.
	cases := []struct {
		name string
		kind string
		in   string
		want string
	}{
		{"replicaset-trimmed", "ReplicaSet", "frontend-7d4f9b8c5", "frontend"},
		{"statefulset-untouched", "StatefulSet", "redis-0", "redis-0"},
		{"daemonset-short-suffix-passthrough", "DaemonSet", "kube-proxy", "kube-proxy"},
		{"empty-name", "ReplicaSet", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeWorkloadName(tc.kind, tc.in); got != tc.want {
				t.Errorf("normalizeWorkloadName(%q, %q)=%q want %q", tc.kind, tc.in, got, tc.want)
			}
		})
	}
}
