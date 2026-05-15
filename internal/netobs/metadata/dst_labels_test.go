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
	ns, wl, uid, _ := c.Labels(podID("ns-a", "p1", "uid-1"))
	if ns != "" || wl != "" || uid != "" {
		t.Errorf("disabled classifier=(ns=%q,wl=%q,uid=%q) want all empty", ns, wl, uid)
	}
}

// TestDstLabelClassifierExternalMarker는 클러스터 외부 IP 흐름이 underscore prefix 규약의 _external
// 합성 라벨로 잡히는지 검증한다. 실제 namespace 이름과 충돌하지 않게 underscore prefix 를 사용한다.
func TestDstLabelClassifierExternalMarker(t *testing.T) {
	c := NewDstLabelClassifier(true, nil)
	ns, wl, uid, _ := c.Labels(externalID())
	if ns != "_external" || wl != "_external" || uid != "" {
		t.Errorf("external=(ns=%q,wl=%q,uid=%q) want (_external,_external,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierUnresolvedFallback는 미해상 dst 흐름이 _unresolved 합성 라벨로 잡히는지
// 검증한다. enricher 가 IP 인덱스에 없는 흐름을 본 classifier 가 silent skip 없이 명시적으로
// "_unresolved" 로 표기하므로 운영자가 미해상 트래픽 비율을 쿼리로 추적 가능하다.
func TestDstLabelClassifierUnresolvedFallback(t *testing.T) {
	c := NewDstLabelClassifier(true, nil)
	ns, wl, uid, _ := c.Labels(unresolvedID())
	if ns != "_unresolved" || wl != "_unresolved" || uid != "" {
		t.Errorf("unresolved=(ns=%q,wl=%q,uid=%q) want (_unresolved,_unresolved,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierServiceLabel는 ClusterIP 로 향하는 흐름이 namespace 와 svc/<name> 형태의
// workload 라벨로 잡히는지 검증한다. backend Pod 으로 추가 해상하지 않고 service 자체를 attribute
// 하는 정책에 따른다.
func TestDstLabelClassifierServiceLabel(t *testing.T) {
	c := NewDstLabelClassifier(true, []string{"default"})
	ns, wl, uid, _ := c.Labels(serviceID("default", "kubernetes"))
	if ns != "default" || wl != "svc/kubernetes" || uid != "" {
		t.Errorf("service=(ns=%q,wl=%q,uid=%q) want (default,svc/kubernetes,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierPodInAllowList는 allow-list 에 등록된 namespace 의 Pod 으로 향하는 dst 가
// PodUID 까지 노출되는지 검증한다. dst_pod_uid 게이트의 positive path 회귀 가드다.
func TestDstLabelClassifierPodInAllowList(t *testing.T) {
	c := NewDstLabelClassifier(true, []string{"ebpf-project", "ns-b"})
	ns, wl, uid, _ := c.Labels(podID("ebpf-project", "agent-1", "uid-abc"))
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
	ns, wl, uid, _ := c.Labels(podID("kube-system", "kube-proxy-1", "uid-xyz"))
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
	ns, wl, uid, _ := c.Labels(podID("any-ns", "any-pod", "uid-1"))
	if ns != "any-ns" || wl != "any-pod" || uid != "" {
		t.Errorf("empty allow-list=(ns=%q,wl=%q,uid=%q) want (any-ns,any-pod,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierCardinalityGate는 이슈 #48 수용 조건 "카디널리티 폭발 안전장치 단위 테스트"
// 의 직접 가드다. 100 개의 서로 다른 PodUID 를 가진 dst Pod 들을 동일 namespace 에 흘렸을 때,
// (a) allow-list 미포함이면 모든 entry 의 UID 가 빈 값으로 collapse 되어 unique 라벨 셋 1개로
// 통제되고, (b) allow-list 포함이면 각 UID 가 그대로 노출되어 100 개 unique 라벨 셋이 됨을 확인
// 한다. cardinality 가 토글로 명시적으로 제어된다는 본 PR 핵심 invariant 의 회귀 가드다.
func TestDstLabelClassifierCardinalityGate(t *testing.T) {
	const n = 100

	// (a) allow-list 미포함: 모든 UID 가 빈 값으로 collapse
	c := NewDstLabelClassifier(true, []string{"other-ns"})
	uidSet := make(map[string]struct{})
	for i := 0; i < n; i++ {
		_, _, uid, _ := c.Labels(podID("kube-system", "pod-x", "uid-"+string(rune('a'+i%26))+"-"+string(rune('0'+i%10))))
		uidSet[uid] = struct{}{}
	}
	if len(uidSet) != 1 {
		t.Errorf("disabled-namespace unique UIDs=%d want 1 (cardinality must collapse to empty)", len(uidSet))
	}
	if _, ok := uidSet[""]; !ok {
		t.Errorf("disabled-namespace UID set=%v want only empty string", uidSet)
	}

	// (b) allow-list 포함: 각 UID 가 그대로 emit 되어 cardinality 가 의도된 만큼 노출
	c = NewDstLabelClassifier(true, []string{"kube-system"})
	uidSet = make(map[string]struct{})
	for i := 0; i < n; i++ {
		uniqueUID := "uid-pod-" + string(rune('a'+(i/26)%26)) + string(rune('a'+i%26))
		_, _, uid, _ := c.Labels(podID("kube-system", "pod-x", uniqueUID))
		uidSet[uid] = struct{}{}
	}
	if len(uidSet) != n {
		t.Errorf("enabled-namespace unique UIDs=%d want %d (each distinct UID should emit)", len(uidSet), n)
	}
}

// TestDstLabelClassifierServiceWithEmptyWorkload는 IdentityClassService 이지만 Workload 가 비어
// 있는 edge 케이스에서 분류기가 panic 없이 PodIdentity.WorkloadLabel 의 fallback ("service") 을
// 그대로 emit 하는지 검증한다. enricher informer race 등으로 잠시 발생 가능한 partial 정체성에
// 대한 회귀 가드다.
func TestDstLabelClassifierServiceWithEmptyWorkload(t *testing.T) {
	c := NewDstLabelClassifier(true, nil)
	ns, wl, uid, _ := c.Labels(kube.PodIdentity{IdentityClass: kube.IdentityClassService, Namespace: "default"})
	if ns != "default" || wl != "service" || uid != "" {
		t.Errorf("service-empty-wl=(ns=%q,wl=%q,uid=%q) want (default,service,\"\")", ns, wl, uid)
	}
}

// TestDstLabelClassifierNodeIdentity는 host network 트래픽 등 IdentityClassNode dst 가 default
// 분기로 떨어져 host / node/<name> 라벨로 emit 되는지 검증한다. UID 게이트와 무관 (Node 는 UID
// 개념 없음) 하므로 dst_pod_uid 는 빈 값으로 유지된다.
func TestDstLabelClassifierNodeIdentity(t *testing.T) {
	c := NewDstLabelClassifier(true, nil)
	ns, wl, uid, _ := c.Labels(kube.PodIdentity{IdentityClass: kube.IdentityClassNode, NodeName: "worker-1"})
	if ns != "host" {
		t.Errorf("node ns=%q want host", ns)
	}
	if wl != "node/worker-1" {
		t.Errorf("node workload=%q want node/worker-1", wl)
	}
	if uid != "" {
		t.Errorf("node uid=%q want empty", uid)
	}
}

// TestDstLabelClassifierOutcomeBuckets는 분류기가 반환하는 outcome bucket 이 6 가지 결정 경로와
// 일대일 매핑되는지 검증한다. self-observe counter netobs_dst_classifier_emits_total{outcome} 의
// 라벨 값이 메트릭 정의와 분류기 결정 로직 사이에서 어긋나지 않게 가드한다.
func TestDstLabelClassifierOutcomeBuckets(t *testing.T) {
	c := NewDstLabelClassifier(true, []string{"allowed-ns"})
	cases := []struct {
		name string
		dst  kube.PodIdentity
		want string
	}{
		{"external", externalID(), OutcomeExternal},
		{"unresolved", unresolvedID(), OutcomeUnresolved},
		{"service", serviceID("default", "kubernetes"), OutcomeService},
		{"pod_with_uid", podID("allowed-ns", "p1", "uid-1"), OutcomePodWithUID},
		{"pod_without_uid", podID("other-ns", "p2", "uid-2"), OutcomePodWithoutUID},
		{"other_node", kube.PodIdentity{IdentityClass: kube.IdentityClassNode, NodeName: "n"}, OutcomeOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, got := c.Labels(tc.dst)
			if got != tc.want {
				t.Errorf("outcome=%q want %q", got, tc.want)
			}
		})
	}

	// disabled classifier 는 어떤 dst 든 outcome=disabled 로 떨어져야 한다.
	disabled := NewDstLabelClassifier(false, []string{"allowed-ns"})
	_, _, _, outcome := disabled.Labels(podID("allowed-ns", "p1", "uid-1"))
	if outcome != OutcomeDisabled {
		t.Errorf("disabled outcome=%q want %q", outcome, OutcomeDisabled)
	}
}

// TestDstLabelClassifierPodWithEmptyNamespace는 Pod 이지만 Namespace 필드가 비어 있는 partial
// 정체성에서 allow-list 매칭이 raw 빈 문자열로 일어나 어떤 운영자 입력에도 매칭되지 않아 UID 가
// emit 되지 않음을 검증한다. parseNamespaceList 가 빈 토큰을 제거하기 때문에 allow-list 에 ""
// 가 들어올 수 없어 이 경로의 dst_pod_uid 는 항상 빈 문자열이다.
func TestDstLabelClassifierPodWithEmptyNamespace(t *testing.T) {
	c := NewDstLabelClassifier(true, []string{"some-ns"})
	ns, wl, uid, _ := c.Labels(kube.PodIdentity{IdentityClass: kube.IdentityClassPod, PodName: "p", PodUID: "uid-1"})
	if uid != "" {
		t.Errorf("empty-ns pod uid=%q want empty (raw namespace cannot match allow-list)", uid)
	}
	// dst_namespace / dst_workload 는 PodIdentity 자체의 fallback ("unknown" / "p") 을 따른다.
	if ns == "" || wl == "" {
		t.Errorf("empty-ns pod ns/wl=(%q,%q) want non-empty fallback labels", ns, wl)
	}
}
