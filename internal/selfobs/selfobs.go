// Package selfobs 는 #405 의 프로세스 자기계측 공용 헬퍼다. 전 서비스 main 이 prometheus.NewRegistry
// 만 써서 go_memstats 와 process_resident_memory 등 표준 프로세스 계측이 전무했고, GOMEMLIMIT
// 미설정으로 컨테이너 메모리 limit 존재를 GC 가 몰랐다.
package selfobs

import (
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// RegisterProcessCollectors 는 Go runtime 과 process 표준 collector 를 reg 에 등록한다 (#405).
// go_goroutines 와 go_memstats_* 와 process_resident_memory_bytes 등이 서비스별 registry 에 실려
// 프로세스 수준 이상 (goroutine 누수, heap 증가, RSS 포화) 을 표준 축으로 관측한다.
func RegisterProcessCollectors(reg prometheus.Registerer) {
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}

// cgroupMemoryMaxPath 는 cgroup v2 의 컨테이너 메모리 limit 파일이다. 테스트가 재지정한다.
var cgroupMemoryMaxPath = "/sys/fs/cgroup/memory.max"

// ApplyMemoryLimit 는 GOMEMLIMIT 미설정 시 cgroup v2 memory limit 의 80% 를 Go soft memory limit
// 으로 설정한다 (#405). limit 존재를 GC 가 알아 OOMKill 전에 회수를 시작한다. GOMEMLIMIT env 가
// 이미 있으면 (Go runtime 이 기동 시 자체 적용) 존중해 건너뛰고, cgroup 파일 부재 (비컨테이너,
// cgroup v1) 나 "max" (무제한) 도 건너뛴다. 20% 헤드룸은 Go heap 밖 소비 (스택, mmap, BPF 맵
// fd 등) 의 몫이다.
func ApplyMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	raw, err := os.ReadFile(cgroupMemoryMaxPath)
	if err != nil {
		log.Printf("selfobs: cgroup memory limit 읽기 생략: %v", err)
		return
	}
	v := strings.TrimSpace(string(raw))
	if v == "max" {
		return
	}
	limit, err := strconv.ParseInt(v, 10, 64)
	if err != nil || limit <= 0 {
		log.Printf("selfobs: cgroup memory limit 파싱 생략: %q", v)
		return
	}
	soft := limit / 10 * 8
	debug.SetMemoryLimit(soft)
	log.Printf("selfobs: GOMEMLIMIT %d bytes 적용 (cgroup limit %d 의 80%%)", soft, limit)
}
