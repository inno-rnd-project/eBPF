// cgroupscan.go 는 #228 의 cgroup id 역매핑 스캐너다. enricher 의 runtimeByCgroup 힌트는 TCP ringbuf
// 이벤트 enrichment 의 부산물로만 학습되어 TCP 트래픽 없이 UDP 만 쓰는 pod 는 pod_bytes / flow_bytes
// 의 cgroup 귀속이 실패한다. 본 스캐너는 informer 의 노드 pod 목록에서 host cgroup2 슬라이스 경로를
// 계산해 디렉터리 inode (= cgroup id) 를 stat 으로 얻어 역매핑 테이블을 주기 재구성하고, enricher 는
// 힌트 캐시 미스 시 본 테이블로 폴백한다.
package metadata

import (
	"context"
	"log"
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
}

// NewCgroupScanner 는 스캐너를 구성한다. kr 이 nil 이면 Lookup 이 항상 miss 를 돌려준다.
func NewCgroupScanner(kr *kube.Resolver, node, root string) *CgroupScanner {
	return &CgroupScanner{kr: kr, node: node, root: root}
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
		for _, ino := range kube.PodCgroupInodes(id.PodUID, c.root) {
			next[ino] = id
		}
	}
	c.table.Store(&next)
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
