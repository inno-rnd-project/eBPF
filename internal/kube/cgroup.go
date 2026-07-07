package kube

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// procCgroupPathFmt는 PID에 대응하는 cgroup membership 파일 경로 포맷이다.
// 테스트에서는 임시 디렉터리로 치환할 수 있도록 변수로 노출한다.
var procCgroupPathFmt = "/proc/%d/cgroup"

// podUIDPattern은 cgroup path 라인에서 Pod UID(8-4-4-4-12 hex)를 추출하는 정규식이다.
// 구분자로 하이픈(cgroupfs driver)과 언더스코어(systemd driver) 양쪽을 허용해
// containerd / docker / cri-o 어느 런타임에서도 매칭한다. 매칭 결과는 normalize 단계에서
// canonical 하이픈 형식으로 통일된다.
var podUIDPattern = regexp.MustCompile(`pod([a-f0-9]{8}[-_][a-f0-9]{4}[-_][a-f0-9]{4}[-_][a-f0-9]{4}[-_][a-f0-9]{12})`)

// readPIDCgroup은 /proc/<pid>/cgroup 파일을 라인 단위로 읽는다.
// 프로세스가 이미 종료되어 파일이 사라진 경우 등은 에러로 반환된다.
func readPIDCgroup(pid uint32) ([]string, error) {
	path := fmt.Sprintf(procCgroupPathFmt, pid)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// extractPodUID는 cgroup 라인 슬라이스를 훑어 첫 번째 매칭된 Pod UID를 canonical(하이픈) 형식으로 반환한다.
// "kubepods"를 포함하지 않는 라인은 host 프로세스로 간주해 건너뛰며,
// 어디에서도 매칭이 없으면 빈 문자열을 반환한다. 동일 Pod의 컨테이너 cgroup은
// 모든 controller line이 같은 Pod UID를 공유하므로 첫 매치를 사용해도 안전하다.
func extractPodUID(lines []string) string {
	for _, line := range lines {
		if !strings.Contains(line, "kubepods") {
			continue
		}
		m := podUIDPattern.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		return strings.ReplaceAll(m[1], "_", "-")
	}
	return ""
}

// PodCgroupInodes 는 #228 의 cgroup id 역매핑 입력이다. Pod UID 로 host cgroup2 계층의 Pod-level
// 슬라이스 후보 경로 (systemd / cgroupfs 드라이버 × QoS class) 를 순회해, 실존하는 슬라이스 디렉터리와
// 그 1단계 자식 (컨테이너 scope) 디렉터리들의 inode 를 돌려준다. cgroup2 에서 cgroup id 는 디렉터리
// inode 번호와 같고, BPF 의 bpf_get_current_cgroup_id 는 컨테이너 scope 의 id 를 주므로 자식까지
// 포함해야 매핑이 성립한다.
func PodCgroupInodes(podUID, root string) []uint64 {
	if podUID == "" {
		return nil
	}
	us := strings.ReplaceAll(podUID, "-", "_")
	candidates := []string{
		// systemd 드라이버
		filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod"+us+".slice"),
		filepath.Join(root, "kubepods.slice", "kubepods-besteffort.slice", "kubepods-besteffort-pod"+us+".slice"),
		filepath.Join(root, "kubepods.slice", "kubepods-pod"+us+".slice"),
		// cgroupfs 드라이버
		filepath.Join(root, "kubepods", "burstable", "pod"+podUID),
		filepath.Join(root, "kubepods", "besteffort", "pod"+podUID),
		filepath.Join(root, "kubepods", "pod"+podUID),
	}
	out := []uint64{}
	for _, dir := range candidates {
		ino, ok := dirInode(dir)
		if !ok {
			continue
		}
		out = append(out, ino)
		entries, err := os.ReadDir(dir)
		if err != nil {
			break
		}
		for _, ent := range entries {
			if !ent.IsDir() {
				continue
			}
			if ino, ok := dirInode(filepath.Join(dir, ent.Name())); ok {
				out = append(out, ino)
			}
		}
		break
	}
	return out
}

// dirInode 는 디렉터리의 inode 번호를 돌려준다. cgroup2 의 cgroup id 와 동일한 값이다.
func dirInode(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ino, true
}
