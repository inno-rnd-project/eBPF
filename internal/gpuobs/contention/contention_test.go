package contention

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParsePSISomeAvg10 은 some 라인 avg10 추출과 full-only / 부재 가드를 검증한다.
func TestParsePSISomeAvg10(t *testing.T) {
	cpu := "some avg10=12.34 avg60=5.00 avg300=1.20 total=11309481\nfull avg10=8.00 avg60=3.00 avg300=0.50 total=8092302\n"
	if v, ok := parsePSISomeAvg10(strings.NewReader(cpu)); !ok || v != 12.34 {
		t.Fatalf("cpu some avg10=%v,%v want 12.34,true", v, ok)
	}
	idle := "some avg10=0.00 avg60=0.00 avg300=0.00 total=10\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=10\n"
	if v, ok := parsePSISomeAvg10(strings.NewReader(idle)); !ok || v != 0 {
		t.Fatalf("idle some avg10=%v,%v want 0,true", v, ok)
	}
	if _, ok := parsePSISomeAvg10(strings.NewReader("full avg10=1.0 total=5\n")); ok {
		t.Fatalf("some 라인 부재인데 ok=true")
	}
}

// TestReadByPodUID 는 Pod UID 로 systemd 드라이버 슬라이스 (하이픈→밑줄, QoS 하위) 를 찾아 cpu / memory
// PSI 를 읽어 0-1 로 정규화하는지, 미존재 UID 는 ok=false 인지 검증한다. host cgroup 계층을 temp dir 로
// 흉내낸다.
func TestReadByPodUID(t *testing.T) {
	root := t.TempDir()
	uid := "40206357-b3f0-4a13-80c1-365204f7a06f"
	// systemd burstable 슬라이스 경로 (하이픈→밑줄).
	slice := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod"+strings.ReplaceAll(uid, "-", "_")+".slice")
	if err := os.MkdirAll(slice, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(slice, "cpu.pressure"),
		"some avg10=93.44 avg60=91.65 avg300=54.17 total=238450366\nfull avg10=80.00 total=1\n")
	writeFile(t, filepath.Join(slice, "memory.pressure"),
		"some avg10=10.00 avg60=5.00 avg300=1.00 total=100\nfull avg10=2.00 total=1\n")
	writeFile(t, filepath.Join(slice, "io.pressure"),
		"some avg10=25.00 avg60=12.00 avg300=3.00 total=500\nfull avg10=20.00 total=2\n")

	st, ok := Read(uid, root)
	if !ok {
		t.Fatalf("Read ok=false want true (슬라이스 존재)")
	}
	if st.CPUPressureRatio < 0.9344-1e-9 || st.CPUPressureRatio > 0.9344+1e-9 {
		t.Errorf("CPUPressureRatio=%v want 0.9344 (93.44/100)", st.CPUPressureRatio)
	}
	if st.MemPressureRatio != 0.1 {
		t.Errorf("MemPressureRatio=%v want 0.1 (10/100)", st.MemPressureRatio)
	}
	if st.IOPressureRatio != 0.25 {
		t.Errorf("IOPressureRatio=%v want 0.25 (25/100)", st.IOPressureRatio)
	}

	if _, ok := Read("no-such-uid", root); ok {
		t.Errorf("미존재 UID 인데 ok=true")
	}
	if _, ok := Read("", root); ok {
		t.Errorf("빈 UID 인데 ok=true")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
