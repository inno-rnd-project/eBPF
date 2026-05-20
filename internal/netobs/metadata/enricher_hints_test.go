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
