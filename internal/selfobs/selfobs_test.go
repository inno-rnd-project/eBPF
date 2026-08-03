package selfobs

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// TestRegisterProcessCollectors 는 표준 collector 2종이 중복 없이 등록되는지 검증한다 (#405).
func TestRegisterProcessCollectors(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterProcessCollectors(reg)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	found := map[string]bool{}
	for _, mf := range mfs {
		found[mf.GetName()] = true
	}
	if !found["go_goroutines"] {
		t.Errorf("go_goroutines 미등록")
	}
}

// TestApplyMemoryLimit 는 cgroup limit 의 80% 가 Go soft limit 으로 설정되는지, "max" 와 GOMEMLIMIT
// 기설정 시 건너뛰는지 검증한다 (#405).
func TestApplyMemoryLimit(t *testing.T) {
	orig := debug.SetMemoryLimit(-1)
	defer debug.SetMemoryLimit(orig)
	origPath := cgroupMemoryMaxPath
	defer func() { cgroupMemoryMaxPath = origPath }()

	dir := t.TempDir()
	f := filepath.Join(dir, "memory.max")
	if err := os.WriteFile(f, []byte("1000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cgroupMemoryMaxPath = f
	ApplyMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != 800000000 {
		t.Errorf("soft limit=%d want 800000000 (80%%)", got)
	}

	// "max" 는 건너뛴다.
	debug.SetMemoryLimit(orig)
	if err := os.WriteFile(f, []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ApplyMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != orig {
		t.Errorf("max 인데 soft limit 변경됨: %d", got)
	}

	// GOMEMLIMIT 기설정은 존중한다.
	t.Setenv("GOMEMLIMIT", "123456789")
	if err := os.WriteFile(f, []byte("1000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ApplyMemoryLimit()
	if got := debug.SetMemoryLimit(-1); got != orig {
		t.Errorf("GOMEMLIMIT 기설정인데 재설정됨: %d", got)
	}
}
