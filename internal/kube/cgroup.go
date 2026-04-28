package kube

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
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
