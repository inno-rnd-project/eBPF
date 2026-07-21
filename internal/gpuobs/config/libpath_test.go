package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveLibPath 는 #296 의 경로 해석 규약을 검증한다. 명시값이 항상 우선하고 (실존 여부 무관),
// 빈 값이면 후보 순회의 첫 실존 경로를, 전부 없으면 빈 문자열을 돌려준다.
func TestResolveLibPath(t *testing.T) {
	dir := t.TempDir()
	hit := filepath.Join(dir, "libcuda.so.1")
	if err := os.WriteFile(hit, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	miss1 := filepath.Join(dir, "없는경로", "libcuda.so.1")
	miss2 := filepath.Join(dir, "없는경로2", "libcuda.so.1")

	// 명시값은 실존하지 않아도 그대로 쓴다 (운영자 의도 존중, 실패는 attach 단계 로그의 몫).
	if got := ResolveLibPath("/etc/명시", []string{hit}); got != "/etc/명시" {
		t.Errorf("explicit=%q want /etc/명시", got)
	}
	// 명시값의 우발적 공백은 정리해 반환한다.
	if got := ResolveLibPath("  /etc/명시  ", []string{hit}); got != "/etc/명시" {
		t.Errorf("trimmed explicit=%q want /etc/명시", got)
	}
	// 첫 실존 후보 채택 (앞선 미실존 후보는 건너뜀).
	if got := ResolveLibPath("", []string{miss1, hit, miss2}); got != hit {
		t.Errorf("resolved=%q want %q", got, hit)
	}
	// 전부 미실존이면 빈 문자열 (호출부 graceful 비활성).
	if got := ResolveLibPath("", []string{miss1, miss2}); got != "" {
		t.Errorf("resolved=%q want empty", got)
	}
}

// TestLibCandidates 는 후보 목록이 Debian multiarch, RHEL lib64, GPU Operator driver 컨테이너
// 순서를 유지하는지 단정한다. DaemonSet 의 /host/usr 와 /host/run/nvidia 마운트 전제와 짝이다.
func TestLibCandidates(t *testing.T) {
	want := []string{
		"/host/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/host/usr/lib64/libcuda.so.1",
		"/host/usr/lib64/nvidia/libcuda.so.1",
		"/host/usr/lib/nvidia/libcuda.so.1",
		"/host/run/nvidia/driver/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		"/host/run/nvidia/driver/usr/lib64/libcuda.so.1",
	}
	got := LibcudaCandidates()
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]=%q want %q", i, got[i], want[i])
		}
	}
	if n := NcclLibCandidates(); n[0] != "/host/usr/lib/x86_64-linux-gnu/libnccl.so.2" {
		t.Errorf("nccl[0]=%q want libnccl multiarch", n[0])
	}
}
