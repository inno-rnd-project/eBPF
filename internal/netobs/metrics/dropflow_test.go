package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestDropFlowGuard_AllowList 는 namespace 가 allow-list 에 없으면 Admit 이 false 반환하는지 검증.
func TestDropFlowGuard_AllowList(t *testing.T) {
	g := NewDropFlowGuard([]string{"correlation-stress"}, 100)
	if g.Admit("not-allowed", "pod-x", "10.0.0.1", 80, "10.0.0.2", 81, "TCP") {
		t.Errorf("not-allowed namespace 가 Admit 됨")
	}
	if !g.Admit("correlation-stress", "pod-x", "10.0.0.1", 80, "10.0.0.2", 81, "TCP") {
		t.Errorf("allowed namespace 가 reject 됨")
	}
}

// TestDropFlowGuard_RejectsZeroIP 는 socket bind 전 drop (saddr/daddr=0) 의 5-tuple 이 LRU 등록
// 거부되는지 검증한다. cache 공간 낭비 방지 가드의 회귀 가드.
func TestDropFlowGuard_RejectsZeroIP(t *testing.T) {
	g := NewDropFlowGuard([]string{"ns"}, 100)
	if g.Admit("ns", "pod-x", "0.0.0.0", 80, "10.0.0.2", 81, "TCP") {
		t.Errorf("0.0.0.0 srcIP 가 admit 됨")
	}
	if g.Admit("ns", "pod-x", "10.0.0.1", 80, "", 81, "TCP") {
		t.Errorf("empty dstIP 가 admit 됨")
	}
	if g.Size() != 0 {
		t.Errorf("size=%d want 0 (reject 후 LRU 비어 있어야 함)", g.Size())
	}
}

// TestDropFlowGuard_EmptyAllowListRejects 는 빈 allow-list 가 모든 emit 을 차단하는지 검증.
// cardinality 안전 default 의 회귀 가드.
func TestDropFlowGuard_EmptyAllowListRejects(t *testing.T) {
	g := NewDropFlowGuard(nil, 100)
	if g.Admit("any-namespace", "pod-x", "10.0.0.1", 80, "10.0.0.2", 81, "TCP") {
		t.Errorf("empty allow-list 가 admit 함 (cardinality 위험)")
	}
}

// TestDropFlowGuard_LRUEvicts 는 maxActive 초과 시 가장 오래된 flow 가 evict 되고 신규는 admit 되는지
// 검증한다.
func TestDropFlowGuard_LRUEvicts(t *testing.T) {
	g := NewDropFlowGuard([]string{"ns"}, 2)
	g.Admit("ns", "pod-x", "10.0.0.1", 1, "10.0.0.2", 1, "TCP")
	g.Admit("ns", "pod-x", "10.0.0.1", 2, "10.0.0.2", 2, "TCP")
	if g.Size() != 2 {
		t.Fatalf("size=%d want 2", g.Size())
	}
	// 3 번째 신규 flow 추가 시 1 번째 evict.
	g.Admit("ns", "pod-x", "10.0.0.1", 3, "10.0.0.2", 3, "TCP")
	if g.Size() != 2 {
		t.Errorf("size=%d want 2 (LRU cap 유지)", g.Size())
	}
}

// TestDropFlowGuard_RevisitMovesToFront 는 동일 flow 의 재방문이 cache hit 으로 처리되어 LRU 의
// 가장 앞으로 이동하는지 검증한다.
func TestDropFlowGuard_RevisitMovesToFront(t *testing.T) {
	g := NewDropFlowGuard([]string{"ns"}, 2)
	g.Admit("ns", "pod-x", "10.0.0.1", 1, "10.0.0.2", 1, "TCP")
	g.Admit("ns", "pod-x", "10.0.0.1", 2, "10.0.0.2", 2, "TCP")
	// flow 1 재방문. evict 대상에서 벗어나야 함.
	g.Admit("ns", "pod-x", "10.0.0.1", 1, "10.0.0.2", 1, "TCP")
	// flow 3 추가 → flow 2 가 evict 되어야 함 (flow 1 은 최근 방문).
	g.Admit("ns", "pod-x", "10.0.0.1", 3, "10.0.0.2", 3, "TCP")
	// flow 1 다시 admit 시도 → 여전히 cache hit 이어야 함.
	if !g.Admit("ns", "pod-x", "10.0.0.1", 1, "10.0.0.2", 1, "TCP") {
		t.Errorf("재방문한 flow 가 cache hit 으로 admit 안 됨")
	}
	if g.Size() != 2 {
		t.Errorf("size=%d want 2", g.Size())
	}
}

// TestDropFlowGuard_StickyCap 은 #403 의 sticky 상한 계약을 검증한다. 상한 도달 후 신규 flow 는
// 거부되고 (종전 evict-후-admit 은 라벨셋을 무한 생성했다), 이미 등록된 flow 는 계속 admit 된다.
func TestDropFlowGuard_StickyCap(t *testing.T) {
	g := NewDropFlowGuard([]string{"ns"}, 2)
	if !g.Admit("ns", "pod-x", "10.0.0.1", 1, "10.0.0.2", 80, "TCP") {
		t.Fatalf("첫 admit 실패")
	}
	if !g.Admit("ns", "pod-x", "10.0.0.1", 2, "10.0.0.2", 80, "TCP") {
		t.Fatalf("둘째 admit 실패")
	}
	if g.Admit("ns", "pod-x", "10.0.0.1", 3, "10.0.0.2", 80, "TCP") {
		t.Errorf("상한 도달 후 신규가 admit 됨 (#403 sticky 상한 위반)")
	}
	if !g.Admit("ns", "pod-x", "10.0.0.1", 1, "10.0.0.2", 80, "TCP") {
		t.Errorf("기존 flow 재방문이 거부됨 (카운터 누적 단절)")
	}
	if g.Size() != 2 {
		t.Errorf("size=%d want 2", g.Size())
	}
}

// dropFlowLabels 는 테스트용 14종 라벨 값 슬라이스를 만든다. src_namespace / src_pod / src_ip 만
// 가변이고 나머지는 고정값이다.
func dropFlowLabels(ns, pod, ip string) []string {
	return []string{"node-a", ns, "wl", pod, "pod_to_pod", "egress", "SKB_DROP", "transport", "TCP", ip, "80", "10.0.0.99", "443", "4"}
}

// TestDropFlowGuard_ReleaseStalePods 는 #407 의 죽은 pod 슬롯/시리즈 회수를 검증한다. sticky 상한
// 아래에서 죽은 pod 의 flow 가 budget 을 영구 점유해 신규 flow 를 막던 상태가, 활성 pod 셋 대조로
// 슬롯이 해제되고 시리즈 (counter + gauge) 도 함께 삭제되어 해소된다.
func TestDropFlowGuard_ReleaseStalePods(t *testing.T) {
	resetMetrics()
	g := NewDropFlowGuard([]string{"ns"}, 2)

	if !g.Admit("ns", "pod-dead", "10.0.0.1", 80, "10.0.0.9", 443, "TCP") {
		t.Fatal("pod-dead admit 실패")
	}
	dropEventsFlow.WithLabelValues(dropFlowLabels("ns", "pod-dead", "10.0.0.1")...).Inc()
	dropLastTimestamp.WithLabelValues(dropFlowLabels("ns", "pod-dead", "10.0.0.1")...).Set(1000)
	if !g.Admit("ns", "pod-live", "10.0.0.2", 80, "10.0.0.9", 443, "TCP") {
		t.Fatal("pod-live admit 실패")
	}
	dropEventsFlow.WithLabelValues(dropFlowLabels("ns", "pod-live", "10.0.0.2")...).Inc()
	dropLastTimestamp.WithLabelValues(dropFlowLabels("ns", "pod-live", "10.0.0.2")...).Set(2000)

	// sticky 상한: 슬롯이 꽉 차 신규 flow 는 거부된다.
	if g.Admit("ns", "pod-new", "10.0.0.3", 80, "10.0.0.9", 443, "TCP") {
		t.Fatal("상한 도달인데 pod-new 가 admit 됨")
	}

	deleted := g.ReleaseStalePods(map[[2]string]struct{}{{"ns", "pod-live"}: {}})
	if deleted != 2 {
		t.Errorf("deleted=%d want 2 (counter 1 + gauge 1)", deleted)
	}
	if g.Size() != 1 {
		t.Errorf("Size=%d want 1 (pod-dead 슬롯 해제)", g.Size())
	}
	if got := testutil.CollectAndCount(dropEventsFlow); got != 1 {
		t.Errorf("drop_events_flow 시리즈=%d want 1 (pod-live 만 잔존)", got)
	}
	if got := testutil.CollectAndCount(dropLastTimestamp); got != 1 {
		t.Errorf("drop_last_timestamp 시리즈=%d want 1", got)
	}

	// 해제된 슬롯으로 신규 flow 가 다시 admit 된다 (guard 상한과 시리즈 수명 일치).
	if !g.Admit("ns", "pod-new", "10.0.0.3", 80, "10.0.0.9", 443, "TCP") {
		t.Error("슬롯 해제 후에도 pod-new 가 거부됨")
	}
}

// TestDropFlowGuard_ReleaseStalePodsPreservesUnattributed 는 pod 이름이 빈 (귀속 불명) entry 가
// 회수 대상에서 제외되는지 검증한다.
func TestDropFlowGuard_ReleaseStalePodsPreservesUnattributed(t *testing.T) {
	resetMetrics()
	g := NewDropFlowGuard([]string{"ns"}, 10)
	if !g.Admit("ns", "", "10.0.0.1", 80, "10.0.0.9", 443, "TCP") {
		t.Fatal("귀속 불명 flow admit 실패")
	}
	if deleted := g.ReleaseStalePods(map[[2]string]struct{}{}); deleted != 0 {
		t.Errorf("deleted=%d want 0", deleted)
	}
	if g.Size() != 1 {
		t.Errorf("Size=%d want 1 (귀속 불명 entry 보존)", g.Size())
	}
}
