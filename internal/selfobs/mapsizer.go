package selfobs

import (
	"errors"

	cebpf "github.com/cilium/ebpf"
)

// MapSizer 는 BPF map 의 현재 entry 수와 max_entries 를 노출한다. cilium/ebpf 의 *cebpf.Map 이 두
// 메서드 (Iterate + MaxEntries via Info) 를 그대로 제공하지만 test seam 을 위해 인터페이스로 둔다.
// netobs selfhealth 와 gpuobs cuda 양쪽의 bpf_map_utilization_ratio 산정이 본 타입을 공유한다 (#413).
type MapSizer interface {
	Entries() (uint64, error)
	MaxEntries() uint64
	Name() string
}

// BPFMapSizer 는 cilium/ebpf 의 *cebpf.Map 을 MapSizer 로 감싼다. Entries 는 next-key iterate 로
// 현재 entry 수를 세고, MaxEntries 는 MapInfo 의 정적 값을 그대로 반환해 BPF 정의가 바뀌어도 Go
// 측 상수 수정 없이 자동 추종된다.
type BPFMapSizer struct {
	m       *cebpf.Map
	name    string
	max     uint64
	keySize uint32
}

// NewBPFMapSizer 는 map handle 로 sizer 를 만든다. nil map 과 Info 실패는 에러로 돌려줘 호출 측이
// 해당 map 만 skip 하는 fail-open 처리를 하게 한다.
func NewBPFMapSizer(name string, m *cebpf.Map) (*BPFMapSizer, error) {
	if m == nil {
		return nil, errors.New("nil bpf map")
	}
	info, err := m.Info()
	if err != nil {
		return nil, err
	}
	return &BPFMapSizer{m: m, name: name, max: uint64(info.MaxEntries), keySize: info.KeySize}, nil
}

func (s *BPFMapSizer) Name() string       { return s.name }
func (s *BPFMapSizer) MaxEntries() uint64 { return s.max }

// Entries 는 BPF map 의 현재 entry 수를 NextKey iterate 로 센다. cilium/ebpf 의 NextKey 가 input
// key 의 길이를 m.keySize 와 정확히 일치시켜 marshal 하므로 cursor / next 두 buffer 를 본 함수
// 진입에서 keySize 만큼 미리 할당해 두고 매 호출마다 copy 로 cursor 를 갱신한다. value lookup 은
// 수행하지 않아 LRU_HASH 와 LRU_PERCPU_HASH 양쪽에 동일 코드가 동작하며, iterate 비용은 단일
// ticker 사이클당 1 회 (최대 수만 entry) 라 scrape hot path 와 분리된 자리에서 무해하다.
func (s *BPFMapSizer) Entries() (uint64, error) {
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
