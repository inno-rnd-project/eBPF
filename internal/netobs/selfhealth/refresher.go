// Package selfhealth 는 netobs agent 의 BPF 자원 포화 신호를 주기적으로 sample 해 metrics 패키지로
// emit 한다. cuda 측 internal/gpuobs/cuda/loader.go 의 droppedBaseline / refreshOnce 패턴을 그대로
// 차용해 events_dropped percpu 카운터의 baseline-then-delta 산정과 LRU 계열 map 의 utilization
// iterate 를 단일 ticker 사이클에 묶는다.
//
// agent main 은 BPF Runtime 이 준비된 시점에 Start 를 호출해 본 패키지의 goroutine 을 spawn 하고,
// ctx cancel 시 자연 종료된다. Run 함수가 Runtime 을 onReady 콜백으로 넘기기 전 단계 (BPF object
// 로드 실패 등) 에서는 본 refresher 가 시작되지 않으므로 metric 은 0 (selfhealth 패키지 미시작) 으로
// 유지되고 운영자가 up{job} 와 program_loaded 메트릭으로 진단을 시작한다.
package selfhealth

import (
	"context"
	"errors"
	"log"
	"time"

	cebpf "github.com/cilium/ebpf"

	"netobs/internal/netobs/metrics"
)

// DefaultRefreshInterval 은 refresher ticker 의 기본 주기다. Prometheus scrape 의 기본 30s 와 정합
// 해 한 scrape 사이클 내에서 최소 1 회 sample 이 보장된다. 별도 tuning 이 필요한 경우 Start 의
// interval 인자로 override 한다.
const DefaultRefreshInterval = 30 * time.Second

// dropSource 는 events_dropped percpu 카운터를 추상화한다. integration test 가 fake counter 를
// 주입할 수 있도록 인터페이스로 노출하고, production 경로는 bpfDropSource 가 *cebpf.Map 을 감싼다.
// cuda 측 droppedSource (cuda/loader.go:441) 와 동일한 자리이며 두 agent 가 동일 패턴을 공유한다.
type dropSource interface {
	Total() uint64
}

// mapSizer 는 BPF map 의 현재 entry 수와 max_entries 를 노출한다. cilium/ebpf 의 *cebpf.Map 이 두
// 메서드 (Iterate + MaxEntries via Info) 를 그대로 제공하지만 test seam 을 위해 인터페이스로 둔다.
type mapSizer interface {
	Entries() (uint64, error)
	MaxEntries() uint64
	Name() string
}

// bpfDropSource 는 production 의 events_dropped 어댑터다.
type bpfDropSource struct {
	m *cebpf.Map
}

func (b bpfDropSource) Total() uint64 { return readDroppedTotal(b.m) }

// bpfMapSizer 는 cilium/ebpf 의 *cebpf.Map 을 mapSizer 로 감싼다. Entries 는 next-key iterate 로
// 현재 entry 수를 세고, MaxEntries 는 MapInfo 의 정적 값을 그대로 반환해 BPF 정의가 바뀌어도 Go
// 측 상수 수정 없이 자동 추종된다.
type bpfMapSizer struct {
	m       *cebpf.Map
	name    string
	max     uint64
	keySize uint32
}

func newBpfMapSizer(name string, m *cebpf.Map) (*bpfMapSizer, error) {
	if m == nil {
		return nil, errors.New("nil bpf map")
	}
	info, err := m.Info()
	if err != nil {
		return nil, err
	}
	return &bpfMapSizer{m: m, name: name, max: uint64(info.MaxEntries), keySize: info.KeySize}, nil
}

func (s *bpfMapSizer) Name() string       { return s.name }
func (s *bpfMapSizer) MaxEntries() uint64 { return s.max }

// Entries 는 BPF map 의 현재 entry 수를 NextKey iterate 로 센다. cilium/ebpf 의 NextKey 가 input
// key 의 길이를 m.keySize 와 정확히 일치시켜 marshal 하므로 cursor / next 두 buffer 를 본 함수
// 진입에서 keySize 만큼 미리 할당해 두고 매 호출마다 copy 로 cursor 를 갱신한다. value lookup 은
// 수행하지 않아 LRU_HASH 와 LRU_PERCPU_HASH 양쪽에 동일 코드가 동작하며, iterate 비용은 단일
// ticker 사이클당 1 회 (최대 16384 entry) 라 scrape hot path 와 분리된 본 자리에서 무해하다.
func (s *bpfMapSizer) Entries() (uint64, error) {
	if s.keySize == 0 {
		return 0, errors.New("invalid map key size")
	}
	var count uint64
	cursor := make([]byte, s.keySize)
	next := make([]byte, s.keySize)
	firstCall := true
	for {
		// 첫 호출은 nil interface 로 NULL 포인터 syscall 을 유도해 첫 키를 받고, 이후 호출은
		// 이전 키를 keySize 정확한 길이의 cursor buffer 로 전달해 marshal length 검증을 통과한다.
		var inKey interface{}
		if !firstCall {
			inKey = cursor
		}
		if err := s.m.NextKey(inKey, &next); err != nil {
			if errors.Is(err, cebpf.ErrKeyNotExist) {
				return count, nil
			}
			return 0, err
		}
		count++
		copy(cursor, next)
		firstCall = false
	}
}

// baseline 은 cuda 측 droppedBaseline (cuda/loader.go:456) 와 동일한 baseline-then-delta 추적기다.
// 첫 호출은 baseline 만 저장하고 add 를 건너뛰며, current < last 인 reset 케이스는 거짓 spike 회피
// 를 위해 가산 skip + 새 baseline 으로 갱신한다.
type baseline struct {
	last        uint64
	initialized bool
}

// Refresher 는 30s ticker 사이클로 events_dropped 의 baseline-then-delta 산정과 LRU map 의
// utilization iterate 를 묶어 metrics 패키지에 emit 하는 단일 책임 컴포넌트다. Start 가 별도
// goroutine 을 spawn 하고, ctx cancel 시 ticker 가 정리된다.
type Refresher struct {
	drops    dropSource
	sizers   []mapSizer
	interval time.Duration
}

// NewRefresher 는 production 경로의 refresher 를 만든다. starts 와 pod_bytes 두 map 의 sizer 구성
// 중 하나가 실패하면 본 함수가 에러를 돌려준다 (agent main 이 self-health 만 실패해도 전체 기동을
// 막지 않도록 호출 측에서 log only 처리할 수 있도록 에러를 노출). dropStacks 는 #83 의 stack trace
// 맵으로 BPF_MAP_TYPE_STACK_TRACE 라 NextKey 동작이 cilium/ebpf 버전에 따라 다를 수 있다. sizer 구성
// 자체는 Info 호출이라 항상 성공하며, Entries() 의 iterate 가 실패하면 refreshOnce 의 기존 log
// + skip 패턴으로 utilization 메트릭만 emit 되지 않는다 (docs/netobs/drop-stack-capture.md 의 fallback
// 명세와 정합).
func NewRefresher(starts, podBytes, eventsDropped, dropStacks *cebpf.Map) (*Refresher, error) {
	startsSizer, err := newBpfMapSizer("starts", starts)
	if err != nil {
		return nil, err
	}
	podBytesSizer, err := newBpfMapSizer("pod_bytes", podBytes)
	if err != nil {
		return nil, err
	}
	sizers := []mapSizer{startsSizer, podBytesSizer}
	if dropStacks != nil {
		if stacksSizer, err := newBpfMapSizer("netobs_drop_stacks", dropStacks); err == nil {
			sizers = append(sizers, stacksSizer)
		} else {
			log.Printf("selfhealth: drop stacks sizer skipped: %v", err)
		}
	}
	return &Refresher{
		drops:    bpfDropSource{m: eventsDropped},
		sizers:   sizers,
		interval: DefaultRefreshInterval,
	}, nil
}

// Start 는 refresher goroutine 을 spawn 한다. 첫 풀링을 즉시 1 회 수행해 첫 scrape 시점에 자료가
// 비어 있지 않게 한 뒤 ticker 주기로 반복한다. ctx cancel 이전에는 함수가 즉시 반환되어 호출 측
// 이 다음 단계를 진행할 수 있다.
func (r *Refresher) Start(ctx context.Context) {
	var b baseline
	r.refreshOnce(&b)
	go r.loop(ctx, &b)
}

func (r *Refresher) loop(ctx context.Context, b *baseline) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refreshOnce(b)
		}
	}
}

// refreshOnce 는 한 ticker 사이클의 모든 작업을 수행한다. cuda 측 refreshOnce (cuda/loader.go:510)
// 의 책임 분리 패턴을 그대로 차용해 통합 테스트가 본 함수를 직접 호출해 사이클 단위 정합성을 검증
// 가능하게 한다.
func (r *Refresher) refreshOnce(b *baseline) {
	current := r.drops.Total()
	switch {
	case !b.initialized:
		b.last = current
		b.initialized = true
	case current < b.last:
		// BPF map reset 등 정상적으로는 일어나기 어려운 케이스. 거짓 spike 회피 위해 가산 skip.
		b.last = current
	case current > b.last:
		metrics.AddBpfRingbufDrops(current - b.last)
		b.last = current
	}

	for _, s := range r.sizers {
		entries, err := s.Entries()
		if err != nil {
			log.Printf("selfhealth: map %s entries iterate: %v", s.Name(), err)
			continue
		}
		max := s.MaxEntries()
		if max == 0 {
			continue
		}
		metrics.SetBpfMapUtilization(s.Name(), float64(entries)/float64(max))
	}
}

// readDroppedTotal 은 events_dropped percpu array (key=0) 슬롯을 모든 CPU 에서 읽어 합산한다.
// cuda 측 readDroppedTotal (cuda/loader.go:547) 와 동일 패턴. lookup 자체가 실패하면 0 을 반환
// 해 delta 가산 없이 무해하게 진행한다.
func readDroppedTotal(m *cebpf.Map) uint64 {
	if m == nil {
		return 0
	}
	var perCPU []uint64
	var key uint32 = 0
	if err := m.Lookup(key, &perCPU); err != nil {
		log.Printf("selfhealth: events_dropped lookup: %v", err)
		return 0
	}
	var total uint64
	for _, v := range perCPU {
		total += v
	}
	return total
}
