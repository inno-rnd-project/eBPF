package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"netobs/internal/kube"
)

// dirIno 는 테스트 헬퍼다. 디렉터리 inode 를 돌려준다.
func dirIno(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// TestPodCgroupInodes 는 systemd 슬라이스 경로에서 pod 슬라이스와 1단계 자식 (컨테이너 scope) 의
// inode 가 모두 수집되는지 검증한다. BPF 의 cgroup id 는 컨테이너 scope 의 inode 라 자식 포함이
// 귀속 성립의 핵심이다.
func TestPodCgroupInodes(t *testing.T) {
	root := t.TempDir()
	uid := "40206357-b3f0-4a13-80c1-365204f7a06f"
	slice := filepath.Join(root, "kubepods.slice", "kubepods-besteffort.slice",
		"kubepods-besteffort-pod"+strings.ReplaceAll(uid, "-", "_")+".slice")
	scope := filepath.Join(slice, "docker-abc123.scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}

	inodes := kube.PodCgroupInodes(uid, root)
	if len(inodes) != 2 {
		t.Fatalf("inodes=%v want 2 (슬라이스 + scope)", inodes)
	}
	want := map[uint64]bool{dirIno(t, slice): true, dirIno(t, scope): true}
	for _, ino := range inodes {
		if !want[ino] {
			t.Errorf("예상 밖 inode %d", ino)
		}
	}
	if got := kube.PodCgroupInodes("no-such-uid", root); len(got) != 0 {
		t.Errorf("미존재 UID inodes=%v want empty", got)
	}
}

// TestEnricher_CgroupScannerFallback 는 힌트 캐시 미스 시 스캐너 테이블로 폴백해 귀속이 성립하는지
// 검증한다. 스캐너가 nil 이거나 테이블 미스면 기존과 동일하게 실패한다.
func TestEnricher_CgroupScannerFallback(t *testing.T) {
	e := NewEnricher(nil)
	if _, ok := e.ResolveCgroup(42); ok {
		t.Fatalf("힌트/폴백 없음에도 ok=true")
	}

	sc := NewCgroupScanner(nil, "n1", "/nonexistent")
	table := map[uint64]kube.PodIdentity{42: {IdentityClass: kube.IdentityClassPod, Namespace: "ns1", PodName: "udp-only", PodUID: "u1", NodeName: "n1"}}
	sc.table.Store(&table)
	e.SetCgroupScanner(sc)

	id, ok := e.ResolveCgroup(42)
	if !ok || id.PodName != "udp-only" {
		t.Errorf("fallback id=%+v ok=%v want udp-only", id, ok)
	}
	if _, ok := e.ResolveCgroup(99); ok {
		t.Errorf("테이블 미스인데 ok=true")
	}
}

// TestCgroupScanner_NilResolver 는 resolver 미주입 시 scan 이 panic 없이 no-op 인지 검증한다.
func TestCgroupScanner_NilResolver(t *testing.T) {
	sc := NewCgroupScanner(nil, "n1", "/nonexistent")
	sc.scan()
	if _, ok := sc.Lookup(1); ok {
		t.Errorf("빈 스캐너인데 Lookup ok=true")
	}
}
