// Package metrics 의 dropflow.go 는 #64 의 netobs_drop_events_flow_total 메트릭에 대한 cardinality
// 가드 (namespace allow-list, top-N LRU sampling) 를 구현한다. 본 라이브러리는 emit 직전에 호출되어
// 가드 통과한 (src_namespace, 5-tuple) 만 메트릭 카운터를 증가시키도록 한다.
package metrics

import (
	"container/list"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
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

// dropFlowEntry 는 LRU element 의 값이다 (#407). srcPod 는 entry 를 만든 flow 의 소유 pod 로,
// ReleaseStalePods 가 죽은 pod 의 슬롯과 시리즈를 함께 회수할 때 대조 키가 된다. 식별 키 (flowKey)
// 에는 넣지 않아 admit dedup 의미는 종전과 동일하다.
type dropFlowEntry struct {
	key    flowKey
	srcPod string
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
// namespace 가 allow-list 에 없으면 false 반환. #403 상한 의미론: 상한 도달 시 신규 flow 는 거부
// 되고 netobs_flow_guard_rejected_total{guard="drop_flow"} 로 계수된다 (sticky 상한). 종전 evict-
// 후-admit 은 신규 라벨셋을 무한 생성해 문서화된 절대 상한 (maxActive * drop_reason 종수) 이
// 동작하지 않았고, CounterVec 시리즈는 evict 로 사라지지 않으므로 신규 거부가 상한을 이행하는
// 유일한 방식이다. 이미 등록된 flow 는 계속 admit 되어 카운터 누적이 이어진다. 빈 IP 또는
// 0.0.0.0 의 5-tuple 은 socket bind 전 drop 으로 정확한 connection 식별이 불가하므로 LRU 등록
// 자체를 거부해 cache 공간이 낭비되지 않게 한다.
func (g *DropFlowGuard) Admit(srcNamespace, srcPod, srcIP string, srcPort uint16, dstIP string, dstPort uint16, protocol string) bool {
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
		AddFlowGuardRejected("drop_flow")
		return false
	}
	e := g.lru.PushFront(&dropFlowEntry{key: k, srcPod: srcPod})
	g.index[k] = e
	return true
}

// ReleaseStalePods 는 활성 pod 셋 (namespace, pod 이름 페어) 에 없는 pod 의 LRU 슬롯을 해제하고
// 해당 pod 의 drop flow 시리즈 (netobs_drop_events_flow_total, netobs_drop_last_timestamp_seconds)
// 를 DeletePartialMatch 로 회수한다 (#407). #403 의 sticky 상한은 슬롯을 스스로 반납하지 않으므로
// 죽은 pod 의 flow 가 budget 을 영구 점유해 신규 flow admit 을 막았고, 시리즈도 에이전트 수명
// 동안 잔존했다. pod 이름이 빈 entry 는 귀속 불명이라 보존한다. 반환값은 삭제한 시리즈 수다.
func (g *DropFlowGuard) ReleaseStalePods(activePods map[[2]string]struct{}) int {
	g.mu.Lock()
	stale := make(map[[2]string]struct{})
	var next *list.Element
	for e := g.lru.Front(); e != nil; e = next {
		next = e.Next()
		entry := e.Value.(*dropFlowEntry)
		if entry.srcPod == "" {
			continue
		}
		nsPod := [2]string{entry.key.srcNamespace, entry.srcPod}
		if _, ok := activePods[nsPod]; ok {
			continue
		}
		stale[nsPod] = struct{}{}
		delete(g.index, entry.key)
		g.lru.Remove(e)
	}
	g.mu.Unlock()

	deleted := 0
	for nsPod := range stale {
		match := prometheus.Labels{"src_namespace": nsPod[0], "src_pod": nsPod[1]}
		deleted += dropEventsFlow.DeletePartialMatch(match)
		deleted += dropLastTimestamp.DeletePartialMatch(match)
	}
	return deleted
}

// Size 는 현재 LRU 가 추적 중인 entry 수를 반환한다. 단위 테스트와 운영 디버깅용.
func (g *DropFlowGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lru.Len()
}
