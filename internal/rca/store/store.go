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

// Store 는 alert 이름 키 인 in-memory map 이다. 동일 alert 이 다시 발화하면 이전 RCASummary 를
// 덮어쓰며, alert 당 최근 1 건만 보관해 메모리 사용량이 alert 수 (9 종) 로 폐쇄적이다.
type Store struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// New 는 빈 Store 를 만든다.
func New() *Store {
	return &Store{entries: make(map[string]Entry)}
}

// Set 은 alert 이름의 최근 RCASummary 를 갱신한다. UpdatedAt 은 본 호출 시점의 wall clock 으로
// 채워진다.
func (s *Store) Set(summary registry.RCASummary) Entry {
	entry := Entry{Summary: summary, UpdatedAt: time.Now()}
	s.mu.Lock()
	s.entries[summary.AlertName] = entry
	s.mu.Unlock()
	return entry
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
