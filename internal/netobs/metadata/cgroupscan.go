// cgroupscan.go 는 #228 의 cgroup id 역매핑 스캐너다. enricher 의 runtimeByCgroup 힌트는 TCP ringbuf
// 이벤트 enrichment 의 부산물로만 학습되어 TCP 트래픽 없이 UDP 만 쓰는 pod 는 pod_bytes / flow_bytes
// 의 cgroup 귀속이 실패한다. 본 스캐너는 informer 의 노드 pod 목록에서 host cgroup2 슬라이스 경로를
// 계산해 디렉터리 inode (= cgroup id) 를 stat 으로 얻어 역매핑 테이블을 주기 재구성하고, enricher 는
// 힌트 캐시 미스 시 본 테이블로 폴백한다.
package metadata

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"netobs/internal/kube"
)

// DefaultCgroupRoot 는 host cgroup2 계층의 컨테이너 내 마운트 경로다. DaemonSet 이 host 의
// /sys/fs/cgroup 을 read-only hostPath 로 bind mount 한다.
const DefaultCgroupRoot = "/host/sys/fs/cgroup"

// CgroupScanner 는 (cgroup id → PodIdentity) 역매핑 테이블을 주기 재구성한다. 테이블은 스캔마다
// 통째로 교체되어 종료 pod 의 stale 엔트리가 다음 스캔에서 자연 소거된다 (기존 힌트 캐시의 TTL
// 체계와는 독립).
type CgroupScanner struct {
	kr    *kube.Resolver
	node  string
	root  string
	table atomic.Pointer[map[uint64]kube.PodIdentity]

	// procRoot 는 소켓 존재 스캔 (#342) 의 /proc 마운트 지점이다. hostPID 컨테이너의 /proc 가
	// 기본값이며 테스트가 임시 디렉터리로 오버라이드한다.
	procRoot string
	// onSocketScan 은 소켓 존재 스캔 결과 hook 이다 (#342). 스캔마다 무소켓 pod 목록과 판별에
	// 성공한 pod 수와 소요 시간을 받으며, nil 이면 소켓 스캔 자체를 건너뛴다.
	onSocketScan func(socketless []kube.PodIdentity, scanned int, dur time.Duration)
}

// NewCgroupScanner 는 스캐너를 구성한다. kr 이 nil 이면 Lookup 이 항상 miss 를 돌려준다.
func NewCgroupScanner(kr *kube.Resolver, node, root string) *CgroupScanner {
	return &CgroupScanner{kr: kr, node: node, root: root, procRoot: "/proc"}
}

// SetSocketScan 은 #342 의 소켓 존재 스캔 결과 hook 을 주입한다. startup 시 1회 호출한다.
func (c *CgroupScanner) SetSocketScan(hook func(socketless []kube.PodIdentity, scanned int, dur time.Duration)) {
	c.onSocketScan = hook
}

// Run 은 주기 스캔 루프다. 첫 스캔을 즉시 수행해 agent 기동 직후의 미해상 창을 줄이고, 첫 스캔
// 완료 직후 테이블 크기를 로그로 남겨 mount / 드라이버 문제를 조기 노출한다.
func (c *CgroupScanner) Run(ctx context.Context, interval time.Duration) {
	c.scan()
	c.LogSize()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.scan()
		}
	}
}

func (c *CgroupScanner) scan() {
	if c.kr == nil {
		return
	}
	pods := c.kr.PodsOnNode(c.node)
	next := make(map[uint64]kube.PodIdentity, len(pods)*3)
	for _, id := range pods {
		for _, ino := range kube.PodCgroupInodes(cgroupScanUID(id), c.root) {
			next[ino] = id
		}
	}
	c.table.Store(&next)
	c.scanSockets(pods)
}

// cgroupScanUID 는 cgroup 디렉터리 탐색에 쓸 UID 다. static pod 는 디렉터리 명이 mirror pod UID
// 가 아닌 config hash 라 CgroupUID 를 우선하고 (#341), 직접 시딩된 identity (테스트 등) 는 PodUID
// 로 폴백한다.
func cgroupScanUID(id kube.PodIdentity) string {
	if id.CgroupUID != "" {
		return id.CgroupUID
	}
	return id.PodUID
}

// scanSockets 는 #342 의 pod 별 소켓 존재 스캔이다. pod slice 의 cgroup.procs 에서 PID 하나를
// 얻어 (컨테이너들이 netns 를 공유하므로 하나면 충분) netns 소켓 테이블 (tcp/tcp6/udp/udp6) 의
// 엔트리 유무를 확인한다. 무소켓 pod 는 TCP/UDP 이벤트가 구조적으로 없어 netobs 시리즈 부재가
// 관측 결함이 아니라는 근거가 된다. hostNetwork pod 는 netns 를 공유해 판별이 무의미하므로
// 제외하고, PID 부재 (종료 pod) 나 읽기 실패 (프로세스 소멸 등) 는 판별 생략으로 graceful 처리해
// 기존 no_data 분류가 유지되게 한다.
func (c *CgroupScanner) scanSockets(pods []kube.PodIdentity) {
	if c.onSocketScan == nil {
		return
	}
	start := time.Now()
	socketless := []kube.PodIdentity{}
	scanned := 0
	for _, id := range pods {
		if id.HostNetwork {
			continue
		}
		pids := kube.PodCgroupPIDs(cgroupScanUID(id), c.root, 1)
		if len(pids) == 0 {
			continue
		}
		has, err := netnsHasSockets(c.procRoot, pids[0])
		if err != nil {
			continue
		}
		scanned++
		if !has {
			socketless = append(socketless, id)
		}
	}
	c.onSocketScan(socketless, scanned, time.Since(start))
}

// netnsHasSockets 는 PID 의 netns 소켓 테이블에 엔트리가 있는지 확인한다. 각 테이블 파일은 헤더
// 1줄을 항상 가지므로 그 이상의 줄이 하나라도 있으면 소켓 존재다. 일부 파일 부재 (프로토콜 비활성
// 커널) 는 건너뛰고, 전부 읽기 실패면 판별 불가로 에러를 돌려준다.
func netnsHasSockets(procRoot string, pid int) (bool, error) {
	read := 0
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "net", table))
		if err != nil {
			continue
		}
		read++
		if strings.Count(strings.TrimRight(string(data), "\n"), "\n") >= 1 {
			return true, nil
		}
	}
	if read == 0 {
		return false, fmt.Errorf("pid %d 의 netns 소켓 테이블 읽기 전부 실패", pid)
	}
	return false, nil
}

// Lookup 은 역매핑 테이블에서 cgroup id 를 해석한다.
func (c *CgroupScanner) Lookup(cgroupID uint64) (kube.PodIdentity, bool) {
	t := c.table.Load()
	if t == nil {
		return kube.PodIdentity{}, false
	}
	id, ok := (*t)[cgroupID]
	return id, ok
}

// LogSize 는 현재 테이블 크기를 로그로 남긴다. Run 이 첫 스캔 완료 직후 호출한다.
func (c *CgroupScanner) LogSize() {
	t := c.table.Load()
	n := 0
	if t != nil {
		n = len(*t)
	}
	log.Printf("cgroup scanner: %d cgroup ids mapped for node %s (root=%s)", n, c.node, c.root)
}
