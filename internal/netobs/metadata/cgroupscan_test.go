package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// TestCgroupScanner_StaticPodConfigHash 는 static pod 의 cgroup 귀속을 검증한다 (#341). kubelet 은
// static pod 의 cgroup 디렉터리를 mirror pod UID 가 아닌 config hash 로 만들므로, 스캐너가
// CgroupUID (config.hash annotation 유래) 를 우선해 inode 를 찾아야 etcd 와 kube-apiserver 귀속이
// 성립한다.
func TestCgroupScanner_StaticPodConfigHash(t *testing.T) {
	root := t.TempDir()
	hash := "b79b63867b08f914e18ce4cb04a1b819"
	slice := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod"+hash+".slice")
	if err := os.MkdirAll(slice, 0o755); err != nil {
		t.Fatal(err)
	}

	// mirror pod UID 로는 디렉터리가 없고 config hash 로만 존재한다.
	if got := kube.PodCgroupInodes("d06f5d6c-02ec-4d10-b32b-bb267e4b6b4c", root); len(got) != 0 {
		t.Fatalf("mirror UID 로 inode 발견: %v (픽스처 오류)", got)
	}
	inodes := kube.PodCgroupInodes(hash, root)
	if len(inodes) != 1 || inodes[0] != dirIno(t, slice) {
		t.Fatalf("config hash inode=%v want 슬라이스 1개", inodes)
	}
}

// TestScanSockets 는 #342 의 pod 소켓 존재 스캔을 검증한다. 무소켓 pod 만 socketless 로 모이고,
// 소켓 있는 pod 와 hostNetwork pod (netns 공유로 판별 무의미) 와 PID 부재 pod (종료 등) 는
// 제외되며, scanned 는 판별 성공 pod 수만 센다.
func TestScanSockets(t *testing.T) {
	cgroupRoot := t.TempDir()
	procRoot := t.TempDir()

	mkPod := func(uid, pid string) {
		scope := filepath.Join(cgroupRoot, "kubepods.slice", "kubepods-besteffort.slice",
			"kubepods-besteffort-pod"+strings.ReplaceAll(uid, "-", "_")+".slice", "cri-x.scope")
		if err := os.MkdirAll(scope, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scope, "cgroup.procs"), []byte(pid+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkNet := func(pid string, tcpEntries int) {
		dir := filepath.Join(procRoot, pid, "net")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		header := "  sl  local_address rem_address   st\n"
		tcp := header
		for i := 0; i < tcpEntries; i++ {
			tcp += "   0: 00000000:0000 00000000:0000 0A\n"
		}
		for f, content := range map[string]string{"tcp": tcp, "tcp6": header, "udp": header, "udp6": header} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	mkPod("aaaaaaaa-0000-0000-0000-000000000001", "100")
	mkNet("100", 0) // 무소켓
	mkPod("aaaaaaaa-0000-0000-0000-000000000002", "200")
	mkNet("200", 2) // 소켓 있음
	mkPod("aaaaaaaa-0000-0000-0000-000000000003", "300")
	// PID 300 의 /proc 부재 (프로세스 소멸 재현) → 판별 생략

	pods := []kube.PodIdentity{
		{IdentityClass: kube.IdentityClassPod, Namespace: "nvdp", PodName: "device-plugin", PodUID: "aaaaaaaa-0000-0000-0000-000000000001"},
		{IdentityClass: kube.IdentityClassPod, Namespace: "app", PodName: "web", PodUID: "aaaaaaaa-0000-0000-0000-000000000002"},
		{IdentityClass: kube.IdentityClassPod, Namespace: "app", PodName: "gone", PodUID: "aaaaaaaa-0000-0000-0000-000000000003"},
		{IdentityClass: kube.IdentityClassPod, Namespace: "kube-system", PodName: "hostnet", PodUID: "aaaaaaaa-0000-0000-0000-000000000004", HostNetwork: true},
	}

	sc := NewCgroupScanner(nil, "n1", cgroupRoot)
	sc.procRoot = procRoot
	var gotSocketless []kube.PodIdentity
	var gotScanned int
	sc.SetSocketScan(func(socketless []kube.PodIdentity, scanned int, dur time.Duration) {
		gotSocketless, gotScanned = socketless, scanned
	})
	sc.scanSockets(pods)

	if len(gotSocketless) != 1 || gotSocketless[0].PodName != "device-plugin" {
		t.Errorf("socketless=%+v want device-plugin 단독", gotSocketless)
	}
	if gotScanned != 2 {
		t.Errorf("scanned=%d want 2 (판별 성공: device-plugin 과 web)", gotScanned)
	}
}
