package mps

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetect_NoSignals 는 env 미설정 + default 경로 부재 + nvidia-cuda-mps-control process 부재 의
// 3종 모두 negative 인 typical dev cluster 환경 에서 false 를 반환 하는지 검증한다.
func TestDetect_NoSignals(t *testing.T) {
	dir := t.TempDir()
	withFSRoot(t, dir)
	t.Setenv("CUDA_MPS_PIPE_DIRECTORY", "")

	// /proc 디렉토리 자체 부재 시에도 false (panic / error propagation 없이 graceful).
	if Detect() {
		t.Fatalf("Detect()=true, want false (no signals)")
	}
}

// TestDetect_PipeDirectoryEnv 는 CUDA_MPS_PIPE_DIRECTORY env 가 가리키는 디렉토리 존재 신호 1종 만으로
// true 를 반환 하는지 검증한다.
func TestDetect_PipeDirectoryEnv(t *testing.T) {
	dir := t.TempDir()
	withFSRoot(t, dir)
	mpsDir := filepath.Join(dir, "custom-mps")
	if err := os.MkdirAll(mpsDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// applyRoot 가 join 하므로 env value 는 root 기준 상대 경로 가 아닌 절대 경로 그대로 둔다.
	t.Setenv("CUDA_MPS_PIPE_DIRECTORY", "/custom-mps")

	if !Detect() {
		t.Fatalf("Detect()=false, want true (CUDA_MPS_PIPE_DIRECTORY hit)")
	}
}

// TestDetect_DefaultLogDir 는 NVIDIA upstream 기본 경로 /var/run/nvidia/mps 존재 신호 만으로 true 를
// 반환 하는지 검증한다.
func TestDetect_DefaultLogDir(t *testing.T) {
	dir := t.TempDir()
	withFSRoot(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "var/run/nvidia/mps"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("CUDA_MPS_PIPE_DIRECTORY", "")

	if !Detect() {
		t.Fatalf("Detect()=false, want true (default log dir hit)")
	}
}

// TestDetect_Process 는 /proc/<pid>/comm 에 nvidia-cuda-mps-control 이 매칭 되는 fake process 가 있을 때
// true 를 반환 하는지 검증한다.
func TestDetect_Process(t *testing.T) {
	dir := t.TempDir()
	withFSRoot(t, dir)
	t.Setenv("CUDA_MPS_PIPE_DIRECTORY", "")

	procDir := filepath.Join(dir, "proc", "12345")
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatalf("mkdir proc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "comm"), []byte("nvidia-cuda-mps-control\n"), 0644); err != nil {
		t.Fatalf("write comm: %v", err)
	}

	if !Detect() {
		t.Fatalf("Detect()=false, want true (process hit)")
	}
}

// TestDetect_NonMpsProcess 는 무관 한 process 이름 만 있는 fake /proc 에서 false 를 반환 하는지 검증해
// false positive 가 발생 하지 않음 을 확인한다.
func TestDetect_NonMpsProcess(t *testing.T) {
	dir := t.TempDir()
	withFSRoot(t, dir)
	t.Setenv("CUDA_MPS_PIPE_DIRECTORY", "")

	procDir := filepath.Join(dir, "proc", "9999")
	if err := os.MkdirAll(procDir, 0755); err != nil {
		t.Fatalf("mkdir proc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(procDir, "comm"), []byte("bash\n"), 0644); err != nil {
		t.Fatalf("write comm: %v", err)
	}

	if Detect() {
		t.Fatalf("Detect()=true, want false (non-mps process)")
	}
}

// TestIsAllDigits 는 /proc PID 디렉토리 식별 helper 의 분기 검증.
func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"12345", true},
		{"1", true},
		{"abc", false},
		{"123abc", false},
		{"abc123", false},
	}
	for _, tc := range cases {
		if got := isAllDigits(tc.in); got != tc.want {
			t.Errorf("isAllDigits(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

// withFSRoot 는 fsRoot 를 t.TempDir 로 격리 한 뒤 테스트 종료 시 원복 한다. detect 분기 테스트 간
// 격리 보장.
func withFSRoot(t *testing.T, dir string) {
	t.Helper()
	orig := fsRoot
	fsRoot = dir
	t.Cleanup(func() { fsRoot = orig })
}
