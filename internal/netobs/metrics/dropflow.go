// Package metrics 의 dropflow.go 는 #64 의 netobs_drop_events_flow_total 메트릭에 대한 cardinality
// 가드 (namespace allow-list, top-N LRU sampling) 를 구현한다. 본 라이브러리는 emit 직전에 호출되어
// 가드 통과한 (src_namespace, 5-tuple) 만 메트릭 카운터를 증가시키도록 한다.
package metrics

import (
	"container/list"
	"sync"
)

// DropFlowGuard 는 namespace allow-list 와 top-N LRU sampling 두 가드를 노출한다. injector_active 와
// 동일하게 단일 cycle 안에서 emit / evict 가 직렬화되어 race 위험은 sync.Mutex 로 보호한다.
type DropFlowGuard struct {
	allowSet map[string]struct{}
	maxN     int

	mu    sync.Mutex
	lru   *list.List
	index map[flowKey]*list.Element
}

// flowKey 는 LRU 의 entry 식별 키다. emit 라벨 셋과 정확히 일치해 동일 5-tuple 의 재방문이 캐시 hit
// 으로 처리된다.
type flowKey struct {
	srcNamespace string
	srcIP        string
	srcPort      uint16
	dstIP        string
	dstPort      uint16
	protocol     string
}

// NewDropFlowGuard 는 namespace allow-list 와 LRU 상한으로 가드를 구성한다. allowList 가 빈 슬라이스
// 면 모든 src_namespace 가 거부된다 (cardinality 안전 default). maxActive 가 0 이하면 1024 로
// fallback 해 LRU eviction 이 비활성화된 무제한 entry 증가 상태를 library 단에서 차단한다.
func NewDropFlowGuard(allowList []string, maxActive int) *DropFlowGuard {
	if maxActive <= 0 {
		maxActive = 1024
	}
	allowSet := make(map[string]struct{}, len(allowList))
	for _, ns := range allowList {
		allowSet[ns] = struct{}{}
	}
	return &DropFlowGuard{
		allowSet: allowSet,
		maxN:     maxActive,
		lru:      list.New(),
		index:    make(map[flowKey]*list.Element, maxActive),
	}
}

// Admit 은 (src_namespace, 5-tuple) 페어가 emit 자격이 있는지 판정하고 LRU 에 등록한다.
// namespace 가 allow-list 에 없으면 false 반환. LRU 상한 초과 시 가장 오래된 entry 가 evict 된 후
// 신규 entry 가 등록되며 신규는 admit 된다. 빈 IP 또는 0.0.0.0 의 5-tuple 은 socket bind 전 drop
// 으로 정확한 connection 식별이 불가하므로 LRU 등록 자체를 거부해 cache 공간이 낭비되지 않게 한다.
func (g *DropFlowGuard) Admit(srcNamespace, srcIP string, srcPort uint16, dstIP string, dstPort uint16, protocol string) bool {
	if _, ok := g.allowSet[srcNamespace]; !ok {
		return false
	}
	if srcIP == "" || srcIP == "0.0.0.0" || dstIP == "" || dstIP == "0.0.0.0" {
		return false
	}
	k := flowKey{srcNamespace, srcIP, srcPort, dstIP, dstPort, protocol}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.index[k]; ok {
		g.lru.MoveToFront(e)
		return true
	}
	if g.lru.Len() >= g.maxN {
		oldest := g.lru.Back()
		if oldest != nil {
			delete(g.index, oldest.Value.(flowKey))
			g.lru.Remove(oldest)
		}
	}
	e := g.lru.PushFront(k)
	g.index[k] = e
	return true
}

// Size 는 현재 LRU 가 추적 중인 entry 수를 반환한다. 단위 테스트와 운영 디버깅용.
func (g *DropFlowGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lru.Len()
}
