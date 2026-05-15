package metadata

import (
	"testing"

	"netobs/internal/kube"
)

func podID(ns, name, uid string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     ns,
		PodName:       name,
		Workload:      name,
		WorkloadKind:  "Deployment",
		PodUID:        uid,
	}
}

func serviceID(ns, name string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassService,
		Namespace:     ns,
		Workload:      name,
	}
}

func externalID() kube.PodIdentity {
	return kube.PodIdentity{IdentityClass: kube.IdentityClassExternal}
}

func unresolvedID() kube.PodIdentity {
	return kube.PodIdentity{IdentityClass: kube.IdentityClassUnresolved}
}

// TestDstLabelClassifierDisabledReturnsEmpty는 master switch 가 꺼지면 dst 라벨 세 칸 모두가 빈
// 문자열로 collapse 되어 cardinality 가 도입 전과 동일하게 유지되는지 검증한다. POD_FLOW_DST_ENABLED
// = false 운영 모드의 회귀 가드다.
func TestDstLabelClassifierDisabledReturnsEmpty(t *testing.T) {
	c := NewDstLabelClassifier(false, []string{"ns-a"})
	ns, wl, uid := c.Labels(podID("ns-a", "p1", "uid-1"))
	if ns != "" || wl != "" || uid != "" {
		t.Errorf("disabled classifier=(ns=%q,wl=%q,uid=%q) want all empty", ns, wl, uid)
	}
}

// TestDstLabelClassifierExternalMarker는 클러스터 외부 IP 흐름이 underscore prefix 규약의 _external
// 합성 라벨로 잡히는지 검증한다. 실제 namespace 이름과 충돌하지 않게 underscore prefix 를 사용한다.
func TestDstLabelClassifierExternalMarker(t *testing.T) {
	c := NewDstLabelClassifier(true, nil)
	ns, wl, uid := c.Labels(externalID())
	if ns != "_external" || wl != "_external" || uid != "" {
		t.Errorf("external=(ns=%q,wl=%q,uid=%q) want (_external,_external,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierUnresolvedFallback는 미해상 dst 흐름이 _unresolved 합성 라벨로 잡히는지
// 검증한다. enricher 가 IP 인덱스에 없는 흐름을 본 classifier 가 silent skip 없이 명시적으로
// "_unresolved" 로 표기하므로 운영자가 미해상 트래픽 비율을 쿼리로 추적 가능하다.
func TestDstLabelClassifierUnresolvedFallback(t *testing.T) {
	c := NewDstLabelClassifier(true, nil)
	ns, wl, uid := c.Labels(unresolvedID())
	if ns != "_unresolved" || wl != "_unresolved" || uid != "" {
		t.Errorf("unresolved=(ns=%q,wl=%q,uid=%q) want (_unresolved,_unresolved,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierServiceLabel는 ClusterIP 로 향하는 흐름이 namespace 와 svc/<name> 형태의
// workload 라벨로 잡히는지 검증한다. backend Pod 으로 추가 해상하지 않고 service 자체를 attribute
// 하는 정책에 따른다.
func TestDstLabelClassifierServiceLabel(t *testing.T) {
	c := NewDstLabelClassifier(true, []string{"default"})
	ns, wl, uid := c.Labels(serviceID("default", "kubernetes"))
	if ns != "default" || wl != "svc/kubernetes" || uid != "" {
		t.Errorf("service=(ns=%q,wl=%q,uid=%q) want (default,svc/kubernetes,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierPodInAllowList는 allow-list 에 등록된 namespace 의 Pod 으로 향하는 dst 가
// PodUID 까지 노출되는지 검증한다. dst_pod_uid 게이트의 positive path 회귀 가드다.
func TestDstLabelClassifierPodInAllowList(t *testing.T) {
	c := NewDstLabelClassifier(true, []string{"ebpf-project", "ns-b"})
	ns, wl, uid := c.Labels(podID("ebpf-project", "agent-1", "uid-abc"))
	if ns != "ebpf-project" || wl != "agent-1" {
		t.Errorf("pod ns/workload=(%q,%q) want (ebpf-project,agent-1)", ns, wl)
	}
	if uid != "uid-abc" {
		t.Errorf("pod uid=%q want uid-abc (namespace is in allow-list)", uid)
	}
}

// TestDstLabelClassifierPodNotInAllowList는 allow-list 에 없는 namespace 의 Pod dst 가 UID 를
// emit 하지 않고 namespace / workload 두 라벨만 노출하는지 검증한다. allow-list 미설정 시 모든 dst
// Pod 시리즈에 UID 가 비어 cardinality 가 통제되는 default 동작의 회귀 가드다.
func TestDstLabelClassifierPodNotInAllowList(t *testing.T) {
	c := NewDstLabelClassifier(true, []string{"ebpf-project"})
	ns, wl, uid := c.Labels(podID("kube-system", "kube-proxy-1", "uid-xyz"))
	if ns != "kube-system" || wl != "kube-proxy-1" {
		t.Errorf("pod ns/workload=(%q,%q) want (kube-system,kube-proxy-1)", ns, wl)
	}
	if uid != "" {
		t.Errorf("pod uid=%q want empty (namespace not in allow-list)", uid)
	}
}

// TestDstLabelClassifierEmptyAllowListSkipsUID는 allow-list 가 빈 슬라이스로 들어왔을 때 (default
// 운영 모드) Pod dst 가 UID 를 emit 하지 않는지 검증한다. enabled=true / allow=nil 조합의 default
// 카디널리티 가드 회귀 보호.
func TestDstLabelClassifierEmptyAllowListSkipsUID(t *testing.T) {
	c := NewDstLabelClassifier(true, nil)
	ns, wl, uid := c.Labels(podID("any-ns", "any-pod", "uid-1"))
	if ns != "any-ns" || wl != "any-pod" || uid != "" {
		t.Errorf("empty allow-list=(ns=%q,wl=%q,uid=%q) want (any-ns,any-pod,\"\")", ns, wl, uid)
	}
}
