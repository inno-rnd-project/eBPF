package metrics

import "testing"

// TestFlowGuard_AllowList 는 namespace 가 allow-list 에 없으면 Admit 이 false 반환 하는 회귀 가드 다.
// cardinality 안전 default 의 핵심 정책.
func TestFlowGuard_AllowList(t *testing.T) {
	g := NewFlowGuard([]string{"observability-test"}, 100)
	if !g.Admit("observability-test", "10.0.0.1", 1234, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("allow-list namespace 가 거부됨")
	}
	if g.Admit("other-ns", "10.0.0.1", 1234, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("allow-list 외 namespace 가 admit 됨")
	}
}

// TestFlowGuard_EmptyAllowListRejects 는 빈 allow-list 가 모든 emit 을 차단 하는 회귀 가드 다.
// cardinality 안전 default.
func TestFlowGuard_EmptyAllowListRejects(t *testing.T) {
	g := NewFlowGuard(nil, 100)
	if g.Admit("ns", "10.0.0.1", 1234, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("empty allow-list 가 admit 함 (cardinality 위험)")
	}
}

// TestFlowGuard_RejectsZeroIP 는 socket bind 전 또는 INADDR_ANY 의 0.0.0.0 5-tuple 이 LRU 등록 자체 를
// 거부 당하는지 확인 해 cache 공간 낭비 를 막는 가드 다.
func TestFlowGuard_RejectsZeroIP(t *testing.T) {
	g := NewFlowGuard([]string{"ns"}, 100)
	if g.Admit("ns", "0.0.0.0", 0, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("0.0.0.0 src 가 admit 됨")
	}
	if g.Admit("ns", "10.0.0.1", 1234, "", 80, "TCP", "egress") {
		t.Errorf("빈 dst 가 admit 됨")
	}
	if g.Size() != 0 {
		t.Errorf("size=%d want 0 (reject 후 LRU 비어 있어야 함)", g.Size())
	}
}

// TestFlowGuard_LRUEvicts 는 maxActive 초과 시 가장 오래된 flow 가 evict 되고 신규 가 admit 되는지
// 검증 한다.
func TestFlowGuard_LRUEvicts(t *testing.T) {
	g := NewFlowGuard([]string{"ns"}, 2)
	g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 80, "TCP", "egress")
	g.Admit("ns", "10.0.0.1", 2, "10.0.0.2", 80, "TCP", "egress")
	g.Admit("ns", "10.0.0.1", 3, "10.0.0.2", 80, "TCP", "egress")
	if g.Size() != 2 {
		t.Errorf("size=%d want 2 (LRU cap 유지)", g.Size())
	}
}

// TestFlowGuard_DirectionSeparate 는 동일 5-tuple 의 egress 와 ingress 가 LRU 에서 별개 entry 로
// 등록 되는지 확인 한다. direction 을 key 에 포함 한 정책 의 회귀 가드.
func TestFlowGuard_DirectionSeparate(t *testing.T) {
	g := NewFlowGuard([]string{"ns"}, 100)
	g.Admit("ns", "10.0.0.1", 1234, "10.0.0.2", 80, "TCP", "egress")
	g.Admit("ns", "10.0.0.1", 1234, "10.0.0.2", 80, "TCP", "ingress")
	if g.Size() != 2 {
		t.Errorf("size=%d want 2 (direction 별 별개 entry)", g.Size())
	}
}

// TestFlowGuard_Allowed 는 빠른 namespace 검사 helper 의 동작 을 검증 한다. flow.Collector 의 iterate
// 진입 직전 가드 비용 절감 patterns.
func TestFlowGuard_Allowed(t *testing.T) {
	g := NewFlowGuard([]string{"ns"}, 100)
	if !g.Allowed("ns") {
		t.Errorf("Allowed(ns)=false want true")
	}
	if g.Allowed("other") {
		t.Errorf("Allowed(other)=true want false")
	}
}
