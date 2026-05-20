package metrics

import "testing"

// TestDropFlowGuard_AllowList 는 namespace 가 allow-list 에 없으면 Admit 이 false 반환하는지 검증.
func TestDropFlowGuard_AllowList(t *testing.T) {
	g := NewDropFlowGuard([]string{"correlation-stress"}, 100)
	if g.Admit("not-allowed", "10.0.0.1", 80, "10.0.0.2", 81, "TCP") {
		t.Errorf("not-allowed namespace 가 Admit 됨")
	}
	if !g.Admit("correlation-stress", "10.0.0.1", 80, "10.0.0.2", 81, "TCP") {
		t.Errorf("allowed namespace 가 reject 됨")
	}
}

// TestDropFlowGuard_EmptyAllowListRejects 는 빈 allow-list 가 모든 emit 을 차단하는지 검증.
// cardinality 안전 default 의 회귀 가드.
func TestDropFlowGuard_EmptyAllowListRejects(t *testing.T) {
	g := NewDropFlowGuard(nil, 100)
	if g.Admit("any-namespace", "10.0.0.1", 80, "10.0.0.2", 81, "TCP") {
		t.Errorf("empty allow-list 가 admit 함 (cardinality 위험)")
	}
}

// TestDropFlowGuard_LRUEvicts 는 maxActive 초과 시 가장 오래된 flow 가 evict 되고 신규는 admit 되는지
// 검증한다.
func TestDropFlowGuard_LRUEvicts(t *testing.T) {
	g := NewDropFlowGuard([]string{"ns"}, 2)
	g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 1, "TCP")
	g.Admit("ns", "10.0.0.1", 2, "10.0.0.2", 2, "TCP")
	if g.Size() != 2 {
		t.Fatalf("size=%d want 2", g.Size())
	}
	// 3 번째 신규 flow 추가 시 1 번째 evict.
	g.Admit("ns", "10.0.0.1", 3, "10.0.0.2", 3, "TCP")
	if g.Size() != 2 {
		t.Errorf("size=%d want 2 (LRU cap 유지)", g.Size())
	}
}

// TestDropFlowGuard_RevisitMovesToFront 는 동일 flow 의 재방문이 cache hit 으로 처리되어 LRU 의
// 가장 앞으로 이동하는지 검증한다.
func TestDropFlowGuard_RevisitMovesToFront(t *testing.T) {
	g := NewDropFlowGuard([]string{"ns"}, 2)
	g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 1, "TCP")
	g.Admit("ns", "10.0.0.1", 2, "10.0.0.2", 2, "TCP")
	// flow 1 재방문. evict 대상에서 벗어나야 함.
	g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 1, "TCP")
	// flow 3 추가 → flow 2 가 evict 되어야 함 (flow 1 은 최근 방문).
	g.Admit("ns", "10.0.0.1", 3, "10.0.0.2", 3, "TCP")
	// flow 1 다시 admit 시도 → 여전히 cache hit 이어야 함.
	if !g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 1, "TCP") {
		t.Errorf("재방문한 flow 가 cache hit 으로 admit 안 됨")
	}
	if g.Size() != 2 {
		t.Errorf("size=%d want 2", g.Size())
	}
}
