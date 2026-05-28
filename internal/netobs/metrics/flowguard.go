// Package metrics 의 flowguard.go 는 #85 의 netobs_flow_bytes_total 메트릭 cardinality 가드 다.
// DropFlowGuard 와 동일 패턴 (namespace allow-list + LRU sampling) 을 따르나 정상 flow 와 drop flow
// 의 admit 결과 가 독립 이라 별도 max_active 로 cap 한다.
package metrics

import (
	"container/list"
	"sync"
)

// FlowGuard 는 #85 의 netobs_flow_bytes_total 메트릭 emit 자격 판정 가드 다. DropFlowGuard 와 동일
// 패턴 (namespace allow-list + LRU sampling) 이지만 정상 flow 의 5-tuple 셋 이 drop flow 와 cross-
// eviction 되지 않도록 별도 LRU 와 별도 max_active 로 분리 한다.
type FlowGuard struct {
	allowSet map[string]struct{}
	maxN     int

	mu    sync.Mutex
	lru   *list.List
	index map[flowGuardKey]*list.Element
}

// flowGuardKey 는 LRU 의 entry 식별 키 다. 정상 flow 는 direction 별 별개 entry 라 key 에 direction 을
// 포함 해 egress 와 ingress 가 동일 LRU 슬롯 을 차지 하지 않도록 한다.
type flowGuardKey struct {
	srcNamespace string
	srcIP        string
	srcPort      uint16
	dstIP        string
	dstPort      uint16
	protocol     string
	direction    string
}

// NewFlowGuard 는 namespace allow-list 와 LRU 상한 으로 가드 를 구성 한다. allowList 가 빈 슬라이스
// 면 모든 src_namespace 가 거부 된다 (cardinality 안전 default). maxActive 가 0 이하 면 1024 로
// fallback 한다.
func NewFlowGuard(allowList []string, maxActive int) *FlowGuard {
	if maxActive <= 0 {
		maxActive = 1024
	}
	allowSet := make(map[string]struct{}, len(allowList))
	for _, ns := range allowList {
		allowSet[ns] = struct{}{}
	}
	return &FlowGuard{
		allowSet: allowSet,
		maxN:     maxActive,
		lru:      list.New(),
		index:    make(map[flowGuardKey]*list.Element, maxActive),
	}
}

// Allowed 는 namespace 가 allow-list 에 포함 되어 있는지 만 검사 한다. flow.Collector 가 scrape 시점 의
// BPF map iterate 진입 직전 에 본 빠른 검사 로 entry 들 의 cardinality 가드 비용 (LRU lookup) 을 우회
// 한다. allow-list 가 비어 있으면 collector 가 iterate 자체 를 skip 한다.
func (g *FlowGuard) Allowed(srcNamespace string) bool {
	_, ok := g.allowSet[srcNamespace]
	return ok
}

// Admit 은 (src_namespace, 5-tuple, direction) 페어 가 emit 자격 이 있는지 판정 하고 LRU 에 등록
// 한다. namespace 가 allow-list 에 없 으면 false 반환. LRU 상한 초과 시 가장 오래된 entry 가 evict
// 된 후 신규 entry 가 등록 되며 신규 는 admit 된다. 빈 IP 또는 0.0.0.0 의 5-tuple 은 socket bind 전
// 또는 INADDR_ANY 로 정확한 connection 식별이 불가 하므로 LRU 등록 자체 를 거부 해 cache 공간 이
// 낭비 되지 않게 한다.
func (g *FlowGuard) Admit(srcNamespace, srcIP string, srcPort uint16, dstIP string, dstPort uint16, protocol, direction string) bool {
	if _, ok := g.allowSet[srcNamespace]; !ok {
		return false
	}
	if srcIP == "" || srcIP == "0.0.0.0" || dstIP == "" || dstIP == "0.0.0.0" {
		return false
	}
	k := flowGuardKey{srcNamespace, srcIP, srcPort, dstIP, dstPort, protocol, direction}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.index[k]; ok {
		g.lru.MoveToFront(e)
		return true
	}
	if g.lru.Len() >= g.maxN {
		oldest := g.lru.Back()
		if oldest != nil {
			delete(g.index, oldest.Value.(flowGuardKey))
			g.lru.Remove(oldest)
		}
	}
	e := g.lru.PushFront(k)
	g.index[k] = e
	return true
}

// Size 는 현재 LRU 가 추적 중인 entry 수 를 반환 한다. 단위 테스트 와 운영 디버깅 용.
func (g *FlowGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lru.Len()
}
