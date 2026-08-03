// Package metrics 의 flowguard.go 는 #85 의 netobs_flow_bytes_total 메트릭 cardinality 가드 다.
// DropFlowGuard 와 유사 패턴 (namespace allow-list + LRU) 을 따르나 정상 flow 와 drop flow 의 admit
// 결과 가 독립 이라 별도 max_active 로 cap 하고, #403 부터 상한 의미론 이 스크레이프당 emit budget
// 이라 세대 (BeginScrape) 개념 을 갖는다.
package metrics

import (
	"container/list"
	"sync"
)

// FlowGuard 는 #85 의 netobs_flow_bytes_total 메트릭 emit 자격 판정 가드 다. 정상 flow 의 5-tuple
// 셋 이 drop flow 와 cross-eviction 되지 않도록 DropFlowGuard 와 별도 LRU 와 별도 max_active 로
// 분리 한다. #403 부터 maxN 은 스크레이프당 emit budget 으로 동작 한다. 종전 evict-후-admit 은
// 신규 를 항상 admit 해 상한 이 스크레이프당 emit 수 를 전혀 제한 하지 못했고 (실측 단일 스크레이프
// 41,654 시리즈), 세대 기반 거부 로 문서화 된 절대 상한 계약 을 이행 한다.
type FlowGuard struct {
	allowSet map[string]struct{}
	maxN     int

	mu    sync.Mutex
	gen   uint64
	lru   *list.List
	index map[flowGuardKey]*list.Element
}

// flowGuardEntry 는 LRU element 의 값 이다. seenGen 은 마지막 으로 Admit 된 scrape 세대 로, 현재
// 세대 에서 관측 되지 않은 stale entry 만 evict 대상 이 되게 한다.
type flowGuardEntry struct {
	key     flowGuardKey
	seenGen uint64
}

// flowGuardKey 는 LRU 의 entry 식별 키 다. 정상 flow 는 direction 별 별개 entry 라 key 에 direction 을
// 포함 해 egress 와 ingress 가 동일 LRU 슬롯 을 차지 하지 않도록 한다.
type flowGuardKey struct {
	srcNamespace string
	srcIP        string
	// srcPort 는 emit 되는 src_port 라벨 문자열 그대로 다 (#403). fold/none 모드 에서 라벨 이 접히면
	// 가드 슬롯 도 함께 접혀 시리즈 identity 와 가드 identity 가 1:1 로 유지 된다.
	srcPort   string
	dstIP     string
	dstPort   uint16
	protocol  string
	direction string
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

// BeginScrape 는 scrape 세대 를 시작 한다 (#403). flow.Collector 가 Collect 진입 시 호출 해 Admit
// 의 스크레이프당 신규 admit 예산 이 세대 단위 로 리셋 되고, 이전 세대 의 stale entry (죽은 flow)
// 가 신규 flow 에 슬롯 을 내주게 한다.
func (g *FlowGuard) BeginScrape() {
	g.mu.Lock()
	g.gen++
	g.mu.Unlock()
}

// Admit 은 (src_namespace, 5-tuple, direction) 페어 가 emit 자격 이 있는지 판정 하고 LRU 에 등록
// 한다. namespace 가 allow-list 에 없 으면 false 반환. #403 상한 의미론: 이미 등록 된 entry 는
// 항상 admit (현재 세대 로 갱신) 되고, 신규 entry 는 LRU 에 여유 가 있거나 가장 오래된 entry 가
// 이전 세대 의 stale 일 때 만 그것 을 evict 하고 admit 된다. 상한 슬롯 전부 가 현재 세대 에서
// 관측 됐으면 (스크레이프당 emit budget 소진) 신규 는 거부 되고 netobs_flow_guard_rejected_total
// {guard="flow"} 로 계수 된다. 이로써 한 scrape 의 admit 키 수 가 maxN 을 넘지 않는다. 빈 IP 또는
// 0.0.0.0 의 5-tuple 은 socket bind 전 또는 INADDR_ANY 로 정확한 connection 식별이 불가 하므로
// LRU 등록 자체 를 거부 해 cache 공간 이 낭비 되지 않게 한다.
func (g *FlowGuard) Admit(srcNamespace, srcIP, srcPort, dstIP string, dstPort uint16, protocol, direction string) bool {
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
		e.Value.(*flowGuardEntry).seenGen = g.gen
		g.lru.MoveToFront(e)
		return true
	}
	if g.lru.Len() >= g.maxN {
		oldest := g.lru.Back()
		if oldest == nil || oldest.Value.(*flowGuardEntry).seenGen >= g.gen {
			AddFlowGuardRejected("flow")
			return false
		}
		delete(g.index, oldest.Value.(*flowGuardEntry).key)
		g.lru.Remove(oldest)
	}
	g.index[k] = g.lru.PushFront(&flowGuardEntry{key: k, seenGen: g.gen})
	return true
}

// Size 는 현재 LRU 가 추적 중인 entry 수 를 반환 한다. 단위 테스트 와 운영 디버깅 용.
func (g *FlowGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lru.Len()
}
