package store

import (
	"strconv"
	"testing"

	"netobs/internal/rca/registry"
)

// TestStore_SetReplacesExistingAlert 는 같은 alertname 이 다시 발화하면 이전 RCASummary 가 덮어
// 쓰이고 entry 수가 그대로 1 인지 검증한다.
func TestStore_SetReplacesExistingAlert(t *testing.T) {
	s := New()
	if _, ok := s.Set(registry.RCASummary{AlertName: "GPUIdleWithCPUThrottle", DominantDimension: "cpu"}); !ok {
		t.Fatalf("first Set ok=false")
	}
	if _, ok := s.Set(registry.RCASummary{AlertName: "GPUIdleWithCPUThrottle", DominantDimension: "gpu"}); !ok {
		t.Fatalf("second Set ok=false")
	}
	if got := s.Len(); got != 1 {
		t.Errorf("Len=%d; want 1 (same alertname must replace)", got)
	}
	entry, _ := s.Get("GPUIdleWithCPUThrottle")
	if entry.Summary.DominantDimension != "gpu" {
		t.Errorf("DominantDimension=%q; want gpu (latest must win)", entry.Summary.DominantDimension)
	}
}

// TestStore_SetCapsAtMaxEntries 는 신규 alertname 이 maxEntries 를 초과하면 거부되고 ok=false 가
// 반환되는지 검증한다. 적대적 webhook 으로 임의 alertname 이 무한 도달해도 메모리가 폐쇄됨을
// 가드하는 회귀 자리다.
func TestStore_SetCapsAtMaxEntries(t *testing.T) {
	s := NewWithMaxEntries(3)
	for i := 0; i < 3; i++ {
		if _, ok := s.Set(registry.RCASummary{AlertName: "alert-" + strconv.Itoa(i)}); !ok {
			t.Fatalf("Set %d ok=false; want true (within cap)", i)
		}
	}
	if got := s.Len(); got != 3 {
		t.Fatalf("Len=%d; want 3", got)
	}
	// 신규 alertname 은 거부되어야 한다.
	if _, ok := s.Set(registry.RCASummary{AlertName: "alert-overflow"}); ok {
		t.Errorf("Set overflow ok=true; want false (cap exceeded)")
	}
	// 기존 alertname 은 cap 무관하게 덮어쓰기 가능해야 한다.
	if _, ok := s.Set(registry.RCASummary{AlertName: "alert-0", DominantDimension: "updated"}); !ok {
		t.Errorf("Set existing alert ok=false; want true (existing must always replace)")
	}
	if got := s.Len(); got != 3 {
		t.Errorf("Len=%d after overflow attempt; want 3", got)
	}
}

// TestStore_NewWithMaxEntriesZeroFallsBackToDefault 는 0 또는 음수 cap 을 주면 default 가 적용
// 되는지 검증한다.
func TestStore_NewWithMaxEntriesZeroFallsBackToDefault(t *testing.T) {
	s := NewWithMaxEntries(0)
	for i := 0; i < DefaultMaxEntries; i++ {
		if _, ok := s.Set(registry.RCASummary{AlertName: "a-" + strconv.Itoa(i)}); !ok {
			t.Fatalf("Set %d ok=false; want true (within default cap)", i)
		}
	}
	if _, ok := s.Set(registry.RCASummary{AlertName: "overflow"}); ok {
		t.Errorf("Set beyond default cap ok=true; want false")
	}
}
