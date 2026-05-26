// Package store 는 alert 이름별 최근 RCASummary 한 건씩만 보관하는 in-memory cache 다.
// cardinality 통제와 /rca 응답의 read hot path 두 책임을 분리해 둔다.
package store

import (
	"sync"
	"time"

	"netobs/internal/rca/registry"
)

// Entry 는 Store 가 보관하는 단일 항목이다. RCASummary 본체와 함께 발생 시각 (UpdatedAt) 을
// 보관해 /rca 응답에서 운영자가 신선도를 판단 가능하게 한다.
type Entry struct {
	Summary   registry.RCASummary `json:"summary"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// DefaultMaxEntries 는 Store 가 보관할 수 있는 최대 alert 종 수다. registry 등록 alert 9 종 +
// 미등록 alert 의 안전 마진을 합쳐 64 로 둔다. 적대적 webhook 으로 임의 alertname 이 무한히
// 들어와도 본 상한에서 거부되어 메모리 사용량이 폐쇄된다. NewWithMaxEntries 로 환경별 override
// 가능.
const DefaultMaxEntries = 64

// Store 는 alert 이름 키 인 in-memory map 이다. 동일 alert 이 다시 발화하면 이전 RCASummary 를
// 덮어쓰며, alert 당 최근 1 건만 보관해 메모리 사용량이 maxEntries 로 폐쇄적이다.
type Store struct {
	mu         sync.RWMutex
	entries    map[string]Entry
	maxEntries int
}

// New 는 DefaultMaxEntries 가드를 가진 빈 Store 를 만든다.
func New() *Store {
	return NewWithMaxEntries(DefaultMaxEntries)
}

// NewWithMaxEntries 는 명시적 cap 으로 Store 를 만든다. 0 이하 값을 주면 DefaultMaxEntries 가
// 적용된다.
func NewWithMaxEntries(max int) *Store {
	if max <= 0 {
		max = DefaultMaxEntries
	}
	return &Store{entries: make(map[string]Entry), maxEntries: max}
}

// Set 은 alert 이름의 최근 RCASummary 를 갱신한다. UpdatedAt 은 본 호출 시점의 wall clock 으로
// 채워진다. 이미 존재하는 alert 이름은 항상 덮어쓰며, 신규 alert 이름은 entries 수가 maxEntries
// 미만일 때만 추가된다. cap 초과 시 신규 entry 는 silent drop 되고 ok=false 가 반환된다.
func (s *Store) Set(summary registry.RCASummary) (Entry, bool) {
	entry := Entry{Summary: summary, UpdatedAt: time.Now()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[summary.AlertName]; !exists {
		if len(s.entries) >= s.maxEntries {
			return Entry{}, false
		}
	}
	s.entries[summary.AlertName] = entry
	return entry, true
}

// Len 은 현재 보관된 entry 수를 돌려준다. 운영 진단 / 단위 테스트에서 cap 동작 검증에 사용된다.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Get 은 alert 이름의 마지막 Entry 를 돌려준다. 미존재 시 ok=false.
func (s *Store) Get(alertname string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[alertname]
	return e, ok
}

// All 은 현재 저장된 모든 Entry 를 alert 이름 기준 정렬되지 않은 슬라이스로 돌려준다. dashboard
// 또는 진단 endpoint 에서 전체 조회용으로 활용한다.
func (s *Store) All() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e)
	}
	return out
}
