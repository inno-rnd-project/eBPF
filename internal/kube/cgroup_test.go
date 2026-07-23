package kube

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalUID는 본 테스트에서 기대 결과로 쓰는 표준 UID(하이픈 형식)다.
const canonicalUID = "d5e3a8f0-4d51-4b0e-8e3d-2a1c4f5a8b9c"

func TestExtractPodUID(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name: "containerd-systemd-cgroup-v2",
			lines: []string{
				"0::/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podd5e3a8f0_4d51_4b0e_8e3d_2a1c4f5a8b9c.slice/cri-containerd-9b5e1c4e8a3d2f1b6a8e7c9d0a1b2c3d.scope",
			},
			want: canonicalUID,
		},
		{
			name: "containerd-systemd-cgroup-v1",
			lines: []string{
				"12:cpu,cpuacct:/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podd5e3a8f0_4d51_4b0e_8e3d_2a1c4f5a8b9c.slice/cri-containerd-abc.scope",
				"11:memory:/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podd5e3a8f0_4d51_4b0e_8e3d_2a1c4f5a8b9c.slice/cri-containerd-abc.scope",
			},
			want: canonicalUID,
		},
		{
			name: "docker-cgroupfs-cgroup-v1",
			lines: []string{
				"12:cpu,cpuacct:/kubepods/besteffort/podd5e3a8f0-4d51-4b0e-8e3d-2a1c4f5a8b9c/abc123",
				"11:memory:/kubepods/besteffort/podd5e3a8f0-4d51-4b0e-8e3d-2a1c4f5a8b9c/abc123",
			},
			want: canonicalUID,
		},
		{
			name: "crio-systemd-cgroup-v2",
			lines: []string{
				"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podd5e3a8f0_4d51_4b0e_8e3d_2a1c4f5a8b9c.slice/crio-9b5e1c4e8a3d2f1b6a8e7c9d.scope",
			},
			want: canonicalUID,
		},
		{
			name: "guaranteed-qos-cgroup-v2",
			lines: []string{
				"0::/kubepods.slice/kubepods-podd5e3a8f0_4d51_4b0e_8e3d_2a1c4f5a8b9c.slice/cri-containerd-abc.scope",
			},
			want: canonicalUID,
		},
		{
			name: "host-process-not-kubepods",
			lines: []string{
				"0::/system.slice/sshd.service",
			},
			want: "",
		},
		{
			name: "kubepods-line-without-uid",
			lines: []string{
				"0::/kubepods.slice/some-other-cgroup",
			},
			want: "",
		},
		{
			name:  "empty-input",
			lines: nil,
			want:  "",
		},
		{
			name: "uid-with-uppercase-hex-not-matched",
			lines: []string{
				"0::/kubepods.slice/kubepods-besteffort-podD5E3A8F0_4D51_4B0E_8E3D_2A1C4F5A8B9C.slice/x.scope",
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractPodUID(tc.lines); got != tc.want {
				t.Errorf("extractPodUID=%q want %q", got, tc.want)
			}
		})
	}
}

// TestReadPIDCgroup_TempFile은 procCgroupPathFmt를 임시 디렉터리로 치환해
// readPIDCgroup의 라인 파싱과 에러 경로를 격리해 검증한다.
func TestReadPIDCgroup_TempFile(t *testing.T) {
	dir := t.TempDir()
	pid := uint32(12345)
	pidDir := filepath.Join(dir, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cgroupContent := "0::/kubepods.slice/kubepods-besteffort-pod" +
		"d5e3a8f0_4d51_4b0e_8e3d_2a1c4f5a8b9c.slice/cri-containerd-abc.scope\n"
	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte(cgroupContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	prevFmt := procCgroupPathFmt
	procCgroupPathFmt = dir + "/%d/cgroup"
	defer func() { procCgroupPathFmt = prevFmt }()

	lines, err := readPIDCgroup(pid)
	if err != nil {
		t.Fatalf("readPIDCgroup: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if got := extractPodUID(lines); got != canonicalUID {
		t.Errorf("extractPodUID via readPIDCgroup=%q want %q", got, canonicalUID)
	}

	// 존재하지 않는 PID는 에러를 돌려준다.
	if _, err := readPIDCgroup(99999); err == nil {
		t.Error("expected error for missing PID file, got nil")
	}
}

// TestPodCgroupPIDs 는 pod slice 와 자식 scope 의 cgroup.procs 에서 PID 를 모으는지 검증한다
// (#342). cgroup v2 는 프로세스가 leaf 에만 속하므로 자식 scope 읽기가 판별 성립의 핵심이다.
func TestPodCgroupPIDs(t *testing.T) {
	root := t.TempDir()
	uid := "40206357-b3f0-4a13-80c1-365204f7a06f"
	slice := filepath.Join(root, "kubepods.slice", "kubepods-besteffort.slice",
		"kubepods-besteffort-pod"+strings.ReplaceAll(uid, "-", "_")+".slice")
	scope := filepath.Join(slice, "cri-containerd-abc.scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	// pod-level 은 internal node 라 비어 있고 leaf scope 에만 PID 가 있다 (cgroup v2 규약 재현).
	if err := os.WriteFile(filepath.Join(slice, "cgroup.procs"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "cgroup.procs"), []byte("3383213\n3383299\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := PodCgroupPIDs(uid, root, 1); len(got) != 1 || got[0] != 3383213 {
		t.Errorf("pids=%v want [3383213] (limit 1)", got)
	}
	if got := PodCgroupPIDs(uid, root, 10); len(got) != 2 {
		t.Errorf("pids=%v want 2개", got)
	}
	if got := PodCgroupPIDs("no-such-uid", root, 1); got != nil {
		t.Errorf("미존재 UID pids=%v want nil (graceful)", got)
	}
}
