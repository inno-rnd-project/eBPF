package metrics

import (
	"sync/atomic"
	"testing"
)

// fakeResolver 는 dropStackResolver 인터페이스 의 테스트 더블 이다. Resolve 호출 시 미리 셋팅 된
// 반환값 을 그대로 돌려 주고 호출 횟수 를 기록 해 본 PR 의 fail-open / cardinality 가드 회귀 가드 에
// 활용한다.
type fakeResolver struct {
	top   string
	hash  string
	ok    bool
	calls int
}

func (f *fakeResolver) Resolve(stackID int32) (string, string, bool) {
	f.calls++
	return f.top, f.hash, f.ok
}

// resetDropStackGlobals 는 패키지 전역 의 guard / resolver / admitter 상태 를 테스트 사이 에 초기화
// 한다. 본 헬퍼 가 없으면 테스트 순서 에 따라 sticky top_function admit set 이 누적 되어 회귀 가드 가
// 깨진다. atomic.Value 는 첫 Store 의 concrete type 을 고정 하므로 빈 값 으로 재할당 해 type 도 함께
// 리셋 한다.
func resetDropStackGlobals() {
	dropStackGuard = nil
	dropStackResolverHandle = atomic.Value{}
	dropStackTopFunctionAdmitter = newTopFunctionAdmitter(64)
}

// TestDropStackGuard_AllowList 는 namespace 가 allow-list 에 없 으면 Admit 이 false 반환 하는지 검증
// 한다. cardinality 안전 default 의 회귀 가드 다.
func TestDropStackGuard_AllowList(t *testing.T) {
	g := NewDropStackGuard([]string{"correlation-stress"}, 100)
	if !g.Admit("correlation-stress", "10.0.0.1", 1234, "10.0.0.2", 5678, "TCP") {
		t.Errorf("allow-list namespace 가 거부됨")
	}
	if g.Admit("other-ns", "10.0.0.1", 1234, "10.0.0.2", 5678, "TCP") {
		t.Errorf("allow-list 외 namespace 가 admit 됨")
	}
}

// TestDropStackGuard_RejectsZeroIP 는 socket bind 전 의 0.0.0.0 5-tuple 이 LRU 등록 자체 를 거부
// 당하는지 확인 해 cache 공간 낭비 를 막는 가드 다.
func TestDropStackGuard_RejectsZeroIP(t *testing.T) {
	g := NewDropStackGuard([]string{"ns"}, 100)
	if g.Admit("ns", "0.0.0.0", 0, "10.0.0.2", 5678, "TCP") {
		t.Errorf("0.0.0.0 src 가 admit 됨")
	}
	if g.Size() != 0 {
		t.Errorf("size=%d want 0 (reject 후 LRU 비어 있어야 함)", g.Size())
	}
}

// TestDropStackGuard_LRUEvicts 는 maxActive 초과 시 가장 오래된 flow 가 evict 되고 신규 가 admit
// 되는지 검증 한다.
func TestDropStackGuard_LRUEvicts(t *testing.T) {
	g := NewDropStackGuard([]string{"ns"}, 2)
	g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 80, "TCP")
	g.Admit("ns", "10.0.0.1", 2, "10.0.0.2", 80, "TCP")
	g.Admit("ns", "10.0.0.1", 3, "10.0.0.2", 80, "TCP")
	if g.Size() != 2 {
		t.Errorf("size=%d want 2 (LRU cap 유지)", g.Size())
	}
}

// TestTopFunctionAdmitter_StickyCap 은 cap 도달 후 신규 name 이 "other" 로 폴딩 되고 기존 admit 된
// name 은 계속 그대로 반환 되는 sticky 정책 의 회귀 가드 다.
func TestTopFunctionAdmitter_StickyCap(t *testing.T) {
	a := newTopFunctionAdmitter(2)
	if got := a.Resolve("tcp_filter"); got != "tcp_filter" {
		t.Errorf("first admit got=%q", got)
	}
	if got := a.Resolve("ip_local_deliver"); got != "ip_local_deliver" {
		t.Errorf("second admit got=%q", got)
	}
	// cap 도달 후 신규 name 은 폴딩.
	if got := a.Resolve("tcp_v4_do_rcv"); got != "other" {
		t.Errorf("overflow got=%q want other", got)
	}
	// 기존 admit 은 sticky.
	if got := a.Resolve("tcp_filter"); got != "tcp_filter" {
		t.Errorf("post-overflow first admit got=%q", got)
	}
}

// TestRecordDropStack_FailOpenOnNilResolver 는 resolver 가 nil 일 때 stack 메트릭 emit 이 skip 되고
// recordDropStack 호출 자체 가 panic 없이 빠져 나오는 fail-open 가드 의 회귀 가드 다.
func TestRecordDropStack_FailOpenOnNilResolver(t *testing.T) {
	resetDropStackGlobals()
	dropStackGuard = NewDropStackGuard([]string{"ns"}, 100)
	// dropStackResolverHandle 은 nil 그대로 유지.

	// panic 없이 즉시 return 되어야 한다.
	recordDropStack("node1", "ns", "wl", "TCP_CLOSE", "tcp", 7)
}

// TestRecordDropStack_SkipsOnResolverNotOK 는 resolver 가 ok=false (음수 stack_id 또는 kallsyms
// resolve 실패) 를 반환 할 때 stack 메트릭 emit 이 skip 되는지 검증 한다. atomic.Value 의 Store 가
// fakeResolver 의 concrete type 을 그대로 보관 하므로 Load 의 type assertion 도 함께 회귀 가드 한다.
func TestRecordDropStack_SkipsOnResolverNotOK(t *testing.T) {
	resetDropStackGlobals()
	dropStackGuard = NewDropStackGuard([]string{"ns"}, 100)
	fr := &fakeResolver{ok: false}
	SetDropStackResolver(fr)

	recordDropStack("node1", "ns", "wl", "TCP_CLOSE", "tcp", 7)
	if fr.calls != 1 {
		t.Errorf("Resolve 호출 횟수 calls=%d want 1", fr.calls)
	}
}
