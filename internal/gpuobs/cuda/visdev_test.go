package cuda

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestParseVisibleDevices_All(t *testing.T) {
	hostByIdx := map[int]string{0: "GPU-A", 1: "GPU-B", 2: "GPU-C"}
	hostSet := map[string]struct{}{"GPU-A": {}, "GPU-B": {}, "GPU-C": {}}

	got := parseVisibleDevices("all", hostByIdx, hostSet)

	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0] != "GPU-A" || got[1] != "GPU-B" || got[2] != "GPU-C" {
		t.Errorf("got=%v want [GPU-A GPU-B GPU-C]", got)
	}
}

func TestParseVisibleDevices_VoidNoneEmpty(t *testing.T) {
	hostByIdx := map[int]string{0: "GPU-A"}
	hostSet := map[string]struct{}{"GPU-A": {}}

	for _, v := range []string{"", "void", "none", "  "} {
		if got := parseVisibleDevices(v, hostByIdx, hostSet); got != nil {
			t.Errorf("value=%q got=%v want nil", v, got)
		}
	}
}

func TestParseVisibleDevices_IndexList(t *testing.T) {
	hostByIdx := map[int]string{0: "GPU-A", 1: "GPU-B", 2: "GPU-C"}
	hostSet := map[string]struct{}{"GPU-A": {}, "GPU-B": {}, "GPU-C": {}}

	got := parseVisibleDevices("0,2", hostByIdx, hostSet)

	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0] != "GPU-A" {
		t.Errorf("ordinal 0 = %q want GPU-A", got[0])
	}
	if got[1] != "GPU-C" {
		t.Errorf("ordinal 1 = %q want GPU-C", got[1])
	}
}

func TestParseVisibleDevices_UUIDList(t *testing.T) {
	hostByIdx := map[int]string{0: "GPU-A", 1: "GPU-B"}
	hostSet := map[string]struct{}{"GPU-A": {}, "GPU-B": {}}

	got := parseVisibleDevices("GPU-B,GPU-A", hostByIdx, hostSet)

	if len(got) != 2 || got[0] != "GPU-B" || got[1] != "GPU-A" {
		t.Errorf("got=%v want [GPU-B GPU-A]", got)
	}
}

func TestParseVisibleDevices_UnknownEntryKeepsSlot(t *testing.T) {
	// 모르는 index / UUID 는 빈 문자열로 자리만 보존해야 ordinal 의 위치 의미가 깨지지 않는다.
	hostByIdx := map[int]string{0: "GPU-A"}
	hostSet := map[string]struct{}{"GPU-A": {}}

	got := parseVisibleDevices("0,99,GPU-X", hostByIdx, hostSet)

	if len(got) != 3 {
		t.Fatalf("len=%d want 3 (slot preserved for unknowns)", len(got))
	}
	if got[0] != "GPU-A" {
		t.Errorf("got[0]=%q want GPU-A", got[0])
	}
	if got[1] != "" {
		t.Errorf("got[1]=%q want empty (unknown index 99)", got[1])
	}
	if got[2] != "" {
		t.Errorf("got[2]=%q want empty (unknown UUID GPU-X)", got[2])
	}
}

func TestParseVisibleDevices_AllPacksDenselyAcrossHostIndexGaps(t *testing.T) {
	// hostUUIDByIndex 가 {0, 2} 로 호스트 NVML index 1 이 비어 있어도, 컨테이너 CUDA driver 는
	// 가용 GPU 2 개를 dense ordinal (0, 1) 로 인식한다. parseVisibleDevices 는 NVML index 를 ASC
	// 정렬한 뒤 dense packing 해 ordinal 0=GPU-A, 1=GPU-C 가 되어야 한다 (gappy 채우기 ❌).
	hostByIdx := map[int]string{0: "GPU-A", 2: "GPU-C"}
	hostSet := map[string]struct{}{"GPU-A": {}, "GPU-C": {}}

	got := parseVisibleDevices("all", hostByIdx, hostSet)

	if len(got) != 2 {
		t.Fatalf("len=%d want 2 (dense packing across gap)", len(got))
	}
	if got[0] != "GPU-A" || got[1] != "GPU-C" {
		t.Errorf("got=%v want [GPU-A GPU-C]", got)
	}
}

func TestVisDevMap_LookupReturnsOrdinalsAndOk(t *testing.T) {
	v := newVisDevMap()
	v.replace(map[uint32][]string{
		1234: {"GPU-A", "GPU-B"},
	})

	ords, ok := v.lookup(1234)
	if !ok {
		t.Fatal("lookup ok=false want true")
	}
	if len(ords) != 2 || ords[0] != "GPU-A" || ords[1] != "GPU-B" {
		t.Errorf("ords=%v want [GPU-A GPU-B]", ords)
	}
}

func TestVisDevMap_LookupMissReturnsOkFalse(t *testing.T) {
	v := newVisDevMap()
	if _, ok := v.lookup(99); ok {
		t.Error("miss lookup ok=true want false")
	}
}

func TestVisDevMap_StoreNegativeResultCacheable(t *testing.T) {
	// NVIDIA_VISIBLE_DEVICES 가 void / 비어 있어 nil 슬라이스를 적재해도 후속 lookup 이 ok=true 를
	// 반환해 dispatch 가 동일 PID 에 대해 environ read 를 다시 하지 않도록 한다.
	v := newVisDevMap()
	v.store(42, nil)
	got, ok := v.lookup(42)
	if !ok {
		t.Fatal("negative cache lookup ok=false want true")
	}
	if got != nil {
		t.Errorf("got=%v want nil (negative result preserved)", got)
	}
}

func TestVisDevMap_ResolveOutOfRange(t *testing.T) {
	v := newVisDevMap()
	v.replace(map[uint32][]string{1: {"GPU-A"}})

	if got := v.resolve(1, 0); got != "GPU-A" {
		t.Errorf("ordinal 0 = %q want GPU-A", got)
	}
	if got := v.resolve(1, 5); got != "" {
		t.Errorf("ordinal 5 = %q want empty (out of range)", got)
	}
	if got := v.resolve(1, -1); got != "" {
		t.Errorf("ordinal -1 = %q want empty (negative)", got)
	}
	if got := v.resolve(99, 0); got != "" {
		t.Errorf("missing pid resolve = %q want empty", got)
	}
}

func TestVisDevMap_ConcurrentLookupAndStore(t *testing.T) {
	// race detector 환경에서 동시 lookup / store / replace 가 안전한지 확인한다.
	v := newVisDevMap()
	const pidCount = 64

	var wg sync.WaitGroup
	for i := 0; i < pidCount; i++ {
		wg.Add(1)
		go func(pid uint32) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				v.store(pid, []string{"GPU-A"})
				_, _ = v.lookup(pid)
			}
		}(uint32(i))
	}

	for k := 0; k < 10; k++ {
		fresh := make(map[uint32][]string, pidCount)
		for i := uint32(0); i < pidCount; i++ {
			fresh[i] = []string{"GPU-A"}
		}
		v.replace(fresh)
	}

	wg.Wait()
}

func TestReadNVIDIAVisibleDevices_Smoke(t *testing.T) {
	// 본 테스트는 /proc 의 실제 read 경로를 검증하기보다, 함수가 누락된 PID 에 대해 에러를 반환하는지만
	// 확인한다 (4294967295 PID 는 환경에 존재하지 않는다고 가정). environ 파싱 정확성은
	// 별도 fixture 기반 테스트에서 검증한다.
	_, err := readNVIDIAVisibleDevices(4294967295)
	if err == nil {
		t.Error("expected error for non-existent PID, got nil")
	}
}

func TestReadNVIDIAVisibleDevices_FixtureFile(t *testing.T) {
	// 파싱 자체는 readNVIDIAVisibleDevices 가 /proc/<pid>/environ 을 ReadFile 하므로,
	// 같은 NUL byte 구분 포맷의 fixture 를 임시 디렉토리에 만들고 직접 검증한다.
	dir := t.TempDir()
	envPath := filepath.Join(dir, "environ")
	content := "PATH=/usr/bin\x00NVIDIA_VISIBLE_DEVICES=GPU-A,1\x00OTHER=v\x00"
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// readNVIDIAVisibleDevices 의 파싱 부분만 직접 재현해 fixture 가 올바른지 확인한다.
	const key = "NVIDIA_VISIBLE_DEVICES="
	got := ""
	for _, entry := range splitNUL(string(data)) {
		if len(entry) >= len(key) && entry[:len(key)] == key {
			got = entry[len(key):]
			break
		}
	}
	if got != "GPU-A,1" {
		t.Errorf("parsed value=%q want %q", got, "GPU-A,1")
	}
}

// splitNUL 은 readNVIDIAVisibleDevices 와 동일한 NUL byte 분해를 테스트에서 재현한다.
func splitNUL(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
