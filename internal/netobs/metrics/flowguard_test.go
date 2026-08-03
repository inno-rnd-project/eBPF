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

// TestFlowGuard_ScrapeBudget 은 #403 의 스크레이프당 emit budget 계약을 검증한다. 한 세대에서
// admit 되는 신규 키가 maxActive 를 넘지 않고 (종전 evict-후-admit 은 전부 admit 했다), budget
// 소진 후 신규는 거부된다.
func TestFlowGuard_ScrapeBudget(t *testing.T) {
	g := NewFlowGuard([]string{"ns"}, 2)
	g.BeginScrape()
	admitted := 0
	for p := uint16(1); p <= 6; p++ {
		if g.Admit("ns", "10.0.0.1", p, "10.0.0.2", 80, "TCP", "egress") {
			admitted++
		}
	}
	if admitted != 2 {
		t.Errorf("admitted=%d want 2 (스크레이프당 budget)", admitted)
	}
	if g.Size() != 2 {
		t.Errorf("size=%d want 2 (LRU cap 유지)", g.Size())
	}
	// 동일 세대에서 이미 등록된 키의 재방문은 budget 소진과 무관하게 admit 된다.
	if !g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("기존 키 재방문이 거부됨")
	}
}

// TestFlowGuard_StaleEvictedNextScrape 는 다음 세대 (BeginScrape) 에서 이전 세대의 stale entry 가
// 신규 flow 에 슬롯을 내주는지 검증한다 (#403). 죽은 flow 가 슬롯을 영구 점유하지 않는다.
func TestFlowGuard_StaleEvictedNextScrape(t *testing.T) {
	g := NewFlowGuard([]string{"ns"}, 2)
	g.BeginScrape()
	g.Admit("ns", "10.0.0.1", 1, "10.0.0.2", 80, "TCP", "egress")
	g.Admit("ns", "10.0.0.1", 2, "10.0.0.2", 80, "TCP", "egress")

	g.BeginScrape()
	// 신규 2건: 이전 세대 stale 2건을 evict 하고 admit 된다.
	if !g.Admit("ns", "10.0.0.9", 3, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("stale evict 후 신규 admit 실패")
	}
	if !g.Admit("ns", "10.0.0.9", 4, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("stale evict 후 두 번째 신규 admit 실패")
	}
	// budget 소진: 세 번째 신규는 거부.
	if g.Admit("ns", "10.0.0.9", 5, "10.0.0.2", 80, "TCP", "egress") {
		t.Errorf("budget 소진 후 신규가 admit 됨 (#403 상한 계약 위반)")
	}
	if g.Size() != 2 {
		t.Errorf("size=%d want 2", g.Size())
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
