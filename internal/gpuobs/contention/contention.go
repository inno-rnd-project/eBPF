// Package contention 은 GPU Pod 의 host 측 cgroup 경합 (PSI pressure stall) 을 수집한다. cAdvisor 가
// 노출하는 CFS throttle 비율 / working-set 대 limit 비율 은 "얼마나 제약에 근접했는가" 의 휴리스틱인
// 반면, cgroup v2 PSI (Pressure Stall Information) 의 cpu.pressure / memory.pressure 는 "실제로 얼마나
// 오래 stall 됐는가" 를 직접 측정한다. #198 의 pod 간 cgroup 경합 (memcg / cpuset 공동피해) 은 이 PSI
// stall 로 직접 관측되며, GPU 유휴 원인 분석 (cgroup_contention cause) 의 입력 신호가 된다.
//
// Pod cgroup 경로는 /proc/<pid>/cgroup 이 아니라 Pod UID 로 직접 구성한다. gpuobs-agent 는 private
// cgroup namespace 라 /proc/<pid>/cgroup 이 agent 자신의 namespace 루트 기준 상대 경로 (../.. 포함) 를
// 돌려줘 host cgroup 마운트와 결합할 수 없기 때문이다. Pod UID 는 systemd / cgroupfs 두 드라이버와 3
// QoS class 에 대해 결정적 슬라이스 경로로 매핑되므로, 후보 경로를 stat 해 첫 존재하는 Pod-level 슬라이스
// 의 PSI 를 읽는다. Pod-level 슬라이스는 Pod 의 모든 컨테이너 stall 을 합산해 "이 Pod 가 굶고 있는가" 를
// 컨테이너 leaf 보다 온전히 반영한다. transient GPU 프로세스 PID 에 의존하지 않아 vectorAdd 처럼 PID 가
// 매 순간 바뀌는 워크로드에서도 안정적으로 읽힌다.
package contention

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultCgroupRoot 는 host cgroup2 계층의 컨테이너 내 마운트 경로다. DaemonSet 이 host 의
// /sys/fs/cgroup 을 read-only hostPath 로 여기 bind mount 한다. 컨테이너 자신의 /sys/fs/cgroup 은
// cgroup namespace 로 격리돼 kubepods.slice 등 타 Pod 경로가 보이지 않으므로 별도 host 마운트가 필요하다.
const DefaultCgroupRoot = "/host/sys/fs/cgroup"

// Stats 는 한 Pod cgroup 의 PSI 압박 비율 (0-1) 이다. cpu.pressure / memory.pressure / io.pressure 의
// some avg10 (최근 10 초 중 하나 이상의 task 가 해당 자원 대기로 stall 된 시간 비율, 커널이 0-100% 로
// 노출) 을 0-1 로 정규화한 값이다. #224 의 io 는 디스크 I/O 대기 stall 로, 간섭 Top-N 의 남은 축인
// 디스크 노이지 네이버 공동피해를 관측한다.
type Stats struct {
	CPUPressureRatio float64
	MemPressureRatio float64
	IOPressureRatio  float64
}

// Read 는 Pod UID 로 host cgroup2 계층의 Pod-level 슬라이스를 찾아 cpu.pressure / memory.pressure /
// io.pressure PSI 를 읽는다. Pod 슬라이스를 못 찾으면 (cgroup v1 / 미동기화 / 종료) ok=false. PSI 파일
// 이 모두 부재하면 ok=false 이고, 일부만 부재하면 (controller 미활성 등) 나머지는 채워 부분 성공을
// 허용한다.
func Read(podUID, cgroupRoot string) (Stats, bool) {
	dir, ok := podCgroupDir(podUID, cgroupRoot)
	if !ok {
		return Stats{}, false
	}

	var st Stats
	got := false
	if v, ok := readPSISomeAvg10(filepath.Join(dir, "cpu.pressure")); ok {
		st.CPUPressureRatio = v / 100
		got = true
	}
	if v, ok := readPSISomeAvg10(filepath.Join(dir, "memory.pressure")); ok {
		st.MemPressureRatio = v / 100
		got = true
	}
	if v, ok := readPSISomeAvg10(filepath.Join(dir, "io.pressure")); ok {
		st.IOPressureRatio = v / 100
		got = true
	}
	return st, got
}

// podCgroupDir 는 Pod UID 에 대응하는 Pod-level cgroup 디렉터리를 후보 경로 stat 으로 찾는다. kubelet
// cgroup 드라이버 (systemd / cgroupfs) 와 QoS class (guaranteed / burstable / besteffort) 조합의
// 결정적 경로를 순회하며 cpu.pressure 가 존재하는 첫 디렉터리를 돌려준다. systemd 드라이버는 슬라이스
// 이름에서 UID 의 하이픈을 밑줄로 바꾸고 (kubepods-burstable-pod<uid_>.slice), guaranteed 는 QoS 하위
// 슬라이스 없이 kubepods.slice 바로 아래 둔다. cgroupfs 드라이버는 raw UID (pod<uid>) 를 쓴다.
func podCgroupDir(podUID, root string) (string, bool) {
	if podUID == "" {
		return "", false
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
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "cpu.pressure")); err == nil {
			return c, true
		}
	}
	return "", false
}

// readPSISomeAvg10 은 PSI 파일 (cpu.pressure / memory.pressure) 을 열어 some 라인의 avg10 값을 돌려준다.
// PSI 미지원 (cgroup v1 / 커널 CONFIG_PSI 비활성) 으로 파일 부재 시 ok=false.
func readPSISomeAvg10(path string) (float64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return parsePSISomeAvg10(f)
}

// parsePSISomeAvg10 은 PSI 포맷 ("some avg10=X.XX avg60=... avg300=... total=...") 에서 some 라인의
// avg10 값 (0-100) 을 파싱한다. some 라인 부재 또는 avg10 파싱 실패 시 ok=false.
func parsePSISomeAvg10(r io.Reader) (float64, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, kv := range fields[1:] {
			if !strings.HasPrefix(kv, "avg10=") {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimPrefix(kv, "avg10="), 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}
