// dropstack.go 는 #83 의 netobs_drop_stack_total 메트릭 과 cardinality 가드 를 구현 한다. 본 메트릭 은
// 기존 netobs_drop_events_labeled_total 가 admit 한 flow 에 한해 top_function 단위 분포 를 추가
// 노출 해 같은 drop_reason 안 의 호출 경로 별 빈도 를 구분 가능 하게 한다.
package metrics

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

// dropStackResolver 는 stack id 를 (top_function, stack_hash) 로 변환 하는 외부 인터페이스 다. main
// agent 가 symbols.Resolver 를 본 인터페이스 형태 로 wire-up 해 metrics 패키지 가 symbols 패키지 에
// 직접 의존 하지 않도록 결합도 를 끊는다.
type dropStackResolver interface {
	Resolve(stackID int32) (topFunction, stackHash string, ok bool)
}

var (
	// dropStackGuard 는 drop stack 메트릭 의 namespace allow-list 와 top-N flow LRU 가드 다. nil 일 때
	// 는 emit 자체 가 skip 되어 cardinality 가 도입 전 수준 (0 series) 으로 유지 된다.
	dropStackGuard *DropStackGuard

	// dropStackResolverHandle 은 main agent 가 SetDropStackResolver 로 주입 하는 symbols.Resolver
	// 핸들 을 atomic.Value 에 보관 한다. onReady 한 번 의 Store 와 event loop 의 다회 Load 패턴 에
	// 대해 channel 송수신 happens-before 외 에 명시 적 동기화 를 제공 해 race detector 와 정합 한다.
	// 비어 있을 때 (Store 미발생) Load 가 nil 을 반환 해 호출 측 의 nil 가드 가 fail-open 분기 를
	// 그대로 탄다.
	dropStackResolverHandle atomic.Value

	// dropStackTopFunctionAdmitter 는 top_function 라벨 cardinality 가드 다. first-N admit 의 sticky
	// 정책 으로 첫 64 개 unique function 만 admit 하고 cap 도달 후 신규 function 은 "other" 로 폴딩
	// 한다.
	dropStackTopFunctionAdmitter = newTopFunctionAdmitter(64)

	// dropStackTotal 은 본 PR 의 신규 메트릭 이다. DropStackGuard.Admit 통과 와 resolver 의 Resolve
	// ok=true 두 조건 을 모두 만족 하는 drop event 에 대해서 만 추가 emit 되며 라벨 셋 은 docs/netobs/
	// drop-stack-capture.md 의 명세 와 정합 한다.
	dropStackTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "netobs_drop_stack_total",
			Help: "Drop events with kernel stack capture for #83. Emitted only when DropStackGuard.Admit allows the flow (namespace allow-list and top-N LRU) and the userspace symbol resolver successfully resolves the top function. top_function is folded to \"other\" beyond the first 64 unique values to cap label cardinality. stack_hash is the hex 8-char form of bpf_get_stackid's u32 return value and is meaningful only within a single BPF program lifetime.",
		},
		[]string{"node", "src_namespace", "src_workload", "drop_reason", "drop_category", "stack_hash", "top_function"},
	)

	// dropStackResolverCacheHits 와 dropStackResolverCacheMisses 는 resolver LRU cache 효율 진단 용
	// counter 다. miss / (hit + miss) 비율 이 지속적으로 높으면 cache cap 부족 또는 stack id churn 의
	// 징후 다. kallsyms 접근 실패 (resolver nil) 케이스 는 본 두 counter 가 둘 다 증가 하지 않으므로
	// resolver health 와 cache health 를 분리 가시화 한다.
	dropStackResolverCacheHits = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "netobs_drop_stack_resolver_cache_hits_total",
			Help: "Cumulative LRU cache hits in the drop stack resolver. Compare against netobs_drop_stack_resolver_cache_misses_total to estimate cache efficiency. Sustained low hit ratio suggests either undersized cache or stack_id churn from frequent BPF program reloads.",
		},
	)
	dropStackResolverCacheMisses = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "netobs_drop_stack_resolver_cache_misses_total",
			Help: "Cumulative LRU cache misses in the drop stack resolver. Each miss triggers a BPF stack_trace map Lookup and kallsyms binary search.",
		},
	)
)

// DropStackGuard 는 #64 의 DropFlowGuard 와 동일 패턴 의 가드 다. drop stack 메트릭 만 의 max_active
// 로 cap 해 DropFlowGuard 와 admit 결과 가 독립 이도록 분리 한다. flow key 도 별도 정의 해 본 가드 의
// LRU 가 drop flow 메트릭 의 가드 와 cross-eviction 되지 않게 한다.
type DropStackGuard struct {
	allowSet map[string]struct{}
	maxN     int

	mu    sync.Mutex
	lru   *list.List
	index map[stackFlowKey]*list.Element
}

type stackFlowKey struct {
	srcNamespace string
	srcIP        string
	srcPort      uint16
	dstIP        string
	dstPort      uint16
	protocol     string
}

// NewDropStackGuard 는 namespace allow-list 와 LRU 상한 으로 가드 를 구성 한다. 빈 allow-list 는 모든
// emit 을 거부 해 cardinality 안전 default 가 된다. maxActive 가 0 이하 면 1024 로 fallback 한다.
func NewDropStackGuard(allowList []string, maxActive int) *DropStackGuard {
	if maxActive <= 0 {
		maxActive = 1024
	}
	allowSet := make(map[string]struct{}, len(allowList))
	for _, ns := range allowList {
		allowSet[ns] = struct{}{}
	}
	return &DropStackGuard{
		allowSet: allowSet,
		maxN:     maxActive,
		lru:      list.New(),
		index:    make(map[stackFlowKey]*list.Element, maxActive),
	}
}

// Admit 은 (src_namespace, 5-tuple) 페어 가 stack 메트릭 emit 자격 이 있는지 판정 하고 LRU 에 등록
// 한다. namespace 가 allow-list 에 없 거나 socket bind 전 의 0.0.0.0 5-tuple 이면 false 반환 으로
// LRU 등록 자체 를 거부 해 cache 공간 이 낭비 되지 않게 한다.
func (g *DropStackGuard) Admit(srcNamespace, srcIP string, srcPort uint16, dstIP string, dstPort uint16, protocol string) bool {
	if _, ok := g.allowSet[srcNamespace]; !ok {
		return false
	}
	if srcIP == "" || srcIP == "0.0.0.0" || dstIP == "" || dstIP == "0.0.0.0" {
		return false
	}
	k := stackFlowKey{srcNamespace, srcIP, srcPort, dstIP, dstPort, protocol}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e, ok := g.index[k]; ok {
		g.lru.MoveToFront(e)
		return true
	}
	if g.lru.Len() >= g.maxN {
		oldest := g.lru.Back()
		if oldest != nil {
			delete(g.index, oldest.Value.(stackFlowKey))
			g.lru.Remove(oldest)
		}
	}
	e := g.lru.PushFront(k)
	g.index[k] = e
	return true
}

// Size 는 현재 LRU 의 entry 수 를 반환 한다. 단위 테스트 와 운영 디버깅 용.
func (g *DropStackGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lru.Len()
}

// topFunctionAdmitter 는 first-N sticky 정책 으로 top_function 라벨 cardinality 를 cap 한다. 첫 N 개
// unique function 까지 admit 하고 cap 도달 후 신규 function 은 "other" 로 폴딩 해 startup 후 추가 된
// 신규 caller frame 의 라벨 폭주 를 차단 한다.
type topFunctionAdmitter struct {
	mu       sync.Mutex
	cap      int
	admit    map[string]struct{}
	overflow string
}

func newTopFunctionAdmitter(cap int) *topFunctionAdmitter {
	if cap <= 0 {
		cap = 64
	}
	return &topFunctionAdmitter{
		cap:      cap,
		admit:    make(map[string]struct{}, cap),
		overflow: "other",
	}
}

// Resolve 는 name 이 admit 되었으면 그대로 반환 하고, admit set 이 비어 있으면 cap 까지 신규 등록 후
// 반환 한다. cap 도달 후 신규 name 은 overflow 라벨 ("other") 로 폴딩 된다.
func (a *topFunctionAdmitter) Resolve(name string) string {
	if name == "" {
		return a.overflow
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.admit[name]; ok {
		return name
	}
	if len(a.admit) >= a.cap {
		return a.overflow
	}
	a.admit[name] = struct{}{}
	return name
}

// SetDropStackGuard 는 main agent 가 startup 시점 에 본 함수 로 가드 를 wire-up 한다. nil 전달 시
// stack 메트릭 emit 이 명시적 으로 disable 된다.
func SetDropStackGuard(g *DropStackGuard) {
	dropStackGuard = g
}

// SetDropStackResolver 는 main agent 가 symbols.Resolver 핸들 을 본 함수 로 주입 한다. resolver init
// 실패 (kallsyms 접근 불가 등) 케이스 에 서 는 main 이 본 함수 를 호출 하지 않 으므로 atomic.Value
// 가 비어 있는 채 로 유지 되어 recordDropStack 의 nil 가드 가 fail-open 분기 를 그대로 탄다.
func SetDropStackResolver(r dropStackResolver) {
	dropStackResolverHandle.Store(r)
}

// IncDropStackResolverCacheHit 와 IncDropStackResolverCacheMiss 는 symbols.Resolver 의 hits / misses
// 콜백 으로 main agent 가 wire-up 하는 export 함수 다. 본 두 함수 가 hit / miss 의 단일 진실 공급원
// 이며 recordDropStack 은 resolver 호출 의 ok 결과 만 본다.
func IncDropStackResolverCacheHit() {
	dropStackResolverCacheHits.Inc()
}

func IncDropStackResolverCacheMiss() {
	dropStackResolverCacheMisses.Inc()
}

// recordDropStack 은 dropEventsLabeled admit 후 stack 메트릭 을 추가 emit 한다. guard 또는 resolver
// 가 nil 이거나 resolver 가 ok=false 를 돌려 주면 emit 자체 가 skip 된다. 본 함수 의 호출 사이트 는
// Record 의 StageDrop 분기 한 곳 뿐 이며 별도 export 하지 않는다. atomic.Value.Load 는 Store 미발생
// 상태 에서 nil 을 반환 하므로 fail-open 분기 가 자연 스럽게 유지 된다.
func recordDropStack(node, srcNs, srcWl, dropReason, dropCategory string,
	stackID int32) {
	resolverVal := dropStackResolverHandle.Load()
	if dropStackGuard == nil || resolverVal == nil {
		return
	}
	resolver, ok := resolverVal.(dropStackResolver)
	if !ok || resolver == nil {
		return
	}
	top, hash, ok := resolver.Resolve(stackID)
	if !ok {
		return
	}
	dropStackTotal.WithLabelValues(
		node,
		srcNs,
		srcWl,
		dropReason,
		dropCategory,
		hash,
		dropStackTopFunctionAdmitter.Resolve(top),
	).Inc()
}
