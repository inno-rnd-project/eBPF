package symbols

import (
	"container/list"
	"testing"
)

// TestPickTopFunction_SkipsKfreeSkbFrames 는 stack[0] 이 kfree_skb_reason 처럼 skipFrames 에 포함 된
// 함수명 일 때 첫 비-skip frame 이 top_function 으로 선택 되는지 검증한다. 본 휴리스틱 이 깨지면 모든
// drop 이 동일 함수명 으로 묶여 변별력 이 사라진다.
func TestPickTopFunction_SkipsKfreeSkbFrames(t *testing.T) {
	tbl := &kallsymsTable{
		syms: []symbol{
			{addr: 0x10, name: "kfree_skb_reason"},
			{addr: 0x20, name: "tcp_filter"},
			{addr: 0x30, name: "tcp_v4_do_rcv"},
		},
	}
	r := &Resolver{table: tbl}

	got := r.pickTopFunction([]uint64{0x10, 0x20, 0x30})
	if got != "tcp_filter" {
		t.Errorf("top=%q want tcp_filter (skipFrames 휴리스틱 미동작)", got)
	}
}

// TestPickTopFunction_AllSkipReturnsEmpty 는 모든 frame 이 skipFrames 에 매칭 되는 비정상 케이스 에서
// 빈 문자열 을 반환 해 호출자 가 ok=false 분기 를 타도록 가드한다.
func TestPickTopFunction_AllSkipReturnsEmpty(t *testing.T) {
	tbl := &kallsymsTable{
		syms: []symbol{
			{addr: 0x10, name: "kfree_skb_reason"},
			{addr: 0x20, name: "handle_kfree_skb_reason"},
			{addr: 0x30, name: "kfree_skb"},
		},
	}
	r := &Resolver{table: tbl}

	if got := r.pickTopFunction([]uint64{0x10, 0x20, 0x30}); got != "" {
		t.Errorf("top=%q want '' (모든 frame skip 이어야 함)", got)
	}
}

// TestResolve_NegativeStackIDFailsOpen 은 BPF helper 가 음수 반환 한 stack_id 에 대해 Resolve 가
// ok=false 를 반환 해 stack 메트릭 emit 이 skip 되도록 가드한다.
func TestResolve_NegativeStackIDFailsOpen(t *testing.T) {
	r := &Resolver{
		table: &kallsymsTable{},
		maxN:  16,
	}
	if _, _, ok := r.Resolve(-1); ok {
		t.Errorf("Resolve(-1)=ok=true (음수 stack_id 가드 미동작)")
	}
	if _, _, ok := r.Resolve(-22); ok {
		t.Errorf("Resolve(-22)=ok=true (음수 stack_id 가드 미동작)")
	}
}

// TestInvalidate_FlushesCache 는 Invalidate 가 LRU 의 모든 entry 를 비워 BPF program reload 후 stale
// stack_id 매핑 이 재사용 되지 않게 가드한다. cacheStore 직접 호출 로 cache 상태 를 세팅 한 뒤 size
// 가 0 으로 떨어지는지 확인한다.
func TestInvalidate_FlushesCache(t *testing.T) {
	r := &Resolver{
		table: &kallsymsTable{},
		maxN:  16,
		lru:   list.New(),
		index: make(map[int32]*list.Element, 16),
	}
	r.cacheStore(7, "tcp_filter", "00000007")
	r.cacheStore(9, "ip_local_deliver", "00000009")
	if r.Size() != 2 {
		t.Fatalf("pre-invalidate size=%d want 2", r.Size())
	}
	r.Invalidate()
	if r.Size() != 0 {
		t.Errorf("post-invalidate size=%d want 0", r.Size())
	}
}
