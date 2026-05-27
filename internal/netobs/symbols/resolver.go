package symbols

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"sync"

	cebpf "github.com/cilium/ebpf"
)

// PerfMaxStackDepth 는 BPF 측 NETOBS_PERF_MAX_STACK_DEPTH 와 정합 하는 frame 배열 길이 다. linux/
// perf_event.h 의 127 frame 상한 을 hardcode 해 vmlinux 의존성 을 회피한다.
const PerfMaxStackDepth = 127

// skipFrames 는 top_function 휴리스틱 의 skip 대상 함수명 셋 이다. kfree_skb_reason 자체 가 항상
// stack[0] 에 잡혀 변별력 이 사라지므로 본 셋 의 함수명 은 stack 앞단 에서 skip 하고 첫 번째 비-skip
// frame 을 top_function 으로 채택 한다.
var skipFrames = map[string]struct{}{
	"kfree_skb_reason":        {},
	"handle_kfree_skb_reason": {},
	"kfree_skb":               {},
}

// Resolver 는 BPF stack_trace 맵 과 kallsyms 를 연동해 stack id 를 (top_function, stack_hash) 로
// 변환한다. LRU cache 로 동일 stack id 의 재방문 비용 을 회피하며 BPF program reload 시 stack id
// 의미 가 reset 되므로 Invalidate 호출 로 cache flush 가 필요 하다.
type Resolver struct {
	table    *kallsymsTable
	stackMap *cebpf.Map

	mu    sync.Mutex
	lru   *list.List
	index map[int32]*list.Element
	maxN  int

	// hits / misses 는 cache 효율 진단 용 counter 다. New 호출자 가 두 nil 함수 를 metrics counter
	// inc 클로저 로 주입 해 외부 의존성 없이 본 패키지 가 metrics 패키지 와 결합 되지 않게 한다.
	hits   func()
	misses func()
}

// resolvedEntry 는 LRU 의 한 항목 이다. stack id 는 list element 의 식별 키 로도 쓰이며 cache 가
// invalidate 될 때 함께 flush 된다.
type resolvedEntry struct {
	stackID     int32
	topFunction string
	stackHash   string
}

// New 는 kallsyms 파일 과 BPF stack_trace 맵 핸들 로 Resolver 를 구성한다. kallsyms 파싱 이 실패
// 하거나 stackMap 이 nil 이면 nil 과 에러 를 반환 해 호출자 가 fail-open 분기 (stack 메트릭 emit
// skip, 다른 메트릭 은 정상 동작) 를 타게 한다. cacheSize 가 0 이하 면 1024 로 fallback 한다.
func New(kallsymsPath string, stackMap *cebpf.Map, cacheSize int, hits, misses func()) (*Resolver, error) {
	if stackMap == nil {
		return nil, fmt.Errorf("stack map 핸들 이 nil 임")
	}
	tbl, err := loadKallsyms(kallsymsPath)
	if err != nil {
		return nil, err
	}
	if cacheSize <= 0 {
		cacheSize = 1024
	}
	return &Resolver{
		table:    tbl,
		stackMap: stackMap,
		lru:      list.New(),
		index:    make(map[int32]*list.Element, cacheSize),
		maxN:     cacheSize,
		hits:     hits,
		misses:   misses,
	}, nil
}

// Resolve 는 stack id 를 (top_function, stack_hash) 로 변환한다. stack_id 가 음수 면 BPF 측 helper
// 의 에러 (-EFAULT 등) 라 ok=false 로 반환 해 호출자 가 stack 메트릭 emit 을 skip 하게 한다. cache
// miss 시 BPF stack_trace 맵 의 Lookup 으로 IP 배열 을 얻고 skipFrames 휴리스틱 으로 top_function
// 을 결정한다. lookup 실패 (예: stale id) 시도 ok=false 로 처리 한다.
func (r *Resolver) Resolve(stackID int32) (topFunction, stackHash string, ok bool) {
	if stackID < 0 {
		return "", "", false
	}

	r.mu.Lock()
	if e, hit := r.index[stackID]; hit {
		entry := e.Value.(*resolvedEntry)
		r.lru.MoveToFront(e)
		r.mu.Unlock()
		if r.hits != nil {
			r.hits()
		}
		return entry.topFunction, entry.stackHash, true
	}
	r.mu.Unlock()
	if r.misses != nil {
		r.misses()
	}

	frames, err := r.lookupFrames(stackID)
	if err != nil || len(frames) == 0 {
		return "", "", false
	}

	top := r.pickTopFunction(frames)
	hash := fmt.Sprintf("%08x", uint32(stackID))
	if top == "" {
		// 모든 frame 이 skipFrames 에 매칭 되거나 kallsyms resolve 가 비어 있는 경우.
		// stack_hash 는 유효 하나 top_function 이 의미 없 으므로 ok=false 로 둔다.
		return "", "", false
	}

	r.cacheStore(stackID, top, hash)
	return top, hash, true
}

// Invalidate 는 LRU cache 를 비운다. BPF program reload 시 stack id 의미 가 reset 되므로 호출자 는
// ebpfReady false 전환 시점 에 본 메서드 를 호출 해 stale id → 잘못된 frame 매핑 회귀 를 막는다.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lru.Init()
	r.index = make(map[int32]*list.Element, r.maxN)
}

// Size 는 현재 cache entry 수 를 반환 한다. 단위 테스트 와 운영 디버깅 용.
func (r *Resolver) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lru.Len()
}

// Base 는 kallsyms 에서 추출 한 _text 심볼 주소 를 반환 한다. 현재 PR 에서는 절대 주소 lookup 만
// 사용 하지만 follow-up 의 KASLR offset 정규화 / cross-reboot stack identity 에서 활용 가능 하다.
func (r *Resolver) Base() uint64 {
	return r.table.base
}

func (r *Resolver) lookupFrames(stackID int32) ([]uint64, error) {
	key := uint32(stackID)
	buf := make([]byte, PerfMaxStackDepth*8)
	if err := r.stackMap.Lookup(key, &buf); err != nil {
		return nil, err
	}
	frames := make([]uint64, 0, PerfMaxStackDepth)
	for i := 0; i < PerfMaxStackDepth; i++ {
		v := binary.NativeEndian.Uint64(buf[i*8 : (i+1)*8])
		if v == 0 {
			break
		}
		frames = append(frames, v)
	}
	return frames, nil
}

// pickTopFunction 은 stack 의 앞단 frame 들 을 순회 하며 skipFrames 에 매칭 되는 함수명 을 skip 하고
// 첫 번째 비-skip frame 의 함수명 을 반환 한다. kallsyms resolve 가 빈 문자열 을 돌려 주면 본 frame
// 은 skip 하지 않고 그대로 빈 결과 로 처리 해 호출자 가 ok=false 분기 를 타게 한다.
func (r *Resolver) pickTopFunction(frames []uint64) string {
	for _, ip := range frames {
		name := r.table.resolve(ip)
		if name == "" {
			continue
		}
		if _, skip := skipFrames[name]; skip {
			continue
		}
		return name
	}
	return ""
}

func (r *Resolver) cacheStore(stackID int32, top, hash string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.index[stackID]; ok {
		r.lru.MoveToFront(e)
		entry := e.Value.(*resolvedEntry)
		entry.topFunction = top
		entry.stackHash = hash
		return
	}
	if r.lru.Len() >= r.maxN {
		oldest := r.lru.Back()
		if oldest != nil {
			delete(r.index, oldest.Value.(*resolvedEntry).stackID)
			r.lru.Remove(oldest)
		}
	}
	entry := &resolvedEntry{stackID: stackID, topFunction: top, stackHash: hash}
	e := r.lru.PushFront(entry)
	r.index[stackID] = e
}
