package cuda

import (
	"sync"
	"testing"

	"netobs/internal/kube"
)

func TestPodMap_LookupReturnsIdentityAndOk(t *testing.T) {
	p := newPodMap()
	p.replace(map[uint32]kube.PodIdentity{
		1234: samplePod("ml", "p1", "u1"),
		5678: samplePod("ml", "p2", "u2"),
	})

	if got, ok := p.lookup(1234); !ok || got.PodName != "p1" {
		t.Errorf("lookup(1234)=%+v ok=%v want p1/true", got, ok)
	}
	if got, ok := p.lookup(5678); !ok || got.PodName != "p2" {
		t.Errorf("lookup(5678)=%+v ok=%v want p2/true", got, ok)
	}
}

func TestPodMap_LookupMissReturnsOkFalse(t *testing.T) {
	p := newPodMap()
	p.replace(map[uint32]kube.PodIdentity{1: samplePod("ml", "p1", "u1")})

	if _, ok := p.lookup(99); ok {
		t.Errorf("lookup(99) ok=true want false (miss)")
	}
}

func TestPodMap_StoreInsertsAndPersists(t *testing.T) {
	// store 는 NVML refresh 사이의 신규 PID 가 두 번째 이벤트부터 hit 경로로 들어가도록 즉석 적재한다.
	p := newPodMap()
	id := samplePod("ml", "p1", "u1")

	if _, ok := p.lookup(42); ok {
		t.Fatalf("pre-store lookup(42) ok=true want false")
	}
	p.store(42, id)
	got, ok := p.lookup(42)
	if !ok || got.PodName != "p1" {
		t.Errorf("post-store lookup(42)=%+v ok=%v want p1/true", got, ok)
	}
}

func TestPodMap_StoreNegativeResultCacheable(t *testing.T) {
	// resolver 가 비-Pod 으로 판정한 PID 도 zero PodIdentity 로 적재되어 후속 lookup 에서 ok=true 로
	// 반환되어야 한다. 이로써 dispatch hot path 가 동일 PID 에 대해 ResolvePID 를 한 번만 호출한다.
	p := newPodMap()
	p.store(99, kube.PodIdentity{})
	got, ok := p.lookup(99)
	if !ok || got.IsPod() {
		t.Errorf("negative store lookup=%+v ok=%v want zero/true", got, ok)
	}
}

func TestPodMap_ReplaceSwapsEntireSnapshot(t *testing.T) {
	// replace 는 통째 교체이므로 이전 snapshot 의 PID 가 빠지면 lookup miss 가 되어야 한다.
	// NVML refresh 사이클에서 종료된 PID 의 stale 캐시 엔트리가 자연 정리되는 메커니즘이다.
	p := newPodMap()
	p.replace(map[uint32]kube.PodIdentity{
		1: samplePod("ml", "p1", "u1"),
		2: samplePod("ml", "p2", "u2"),
	})
	p.replace(map[uint32]kube.PodIdentity{
		2: samplePod("ml", "p2", "u2"),
	})

	if _, ok := p.lookup(1); ok {
		t.Errorf("after replace, removed pid lookup ok=true want false")
	}
	if got, ok := p.lookup(2); !ok || got.PodName != "p2" {
		t.Errorf("survived pid lookup=%+v ok=%v want p2/true", got, ok)
	}
}

func TestPodMap_EmptyOnInit(t *testing.T) {
	p := newPodMap()
	if _, ok := p.lookup(1); ok {
		t.Errorf("fresh podmap lookup ok=true want false")
	}
}

func TestPodMap_ConcurrentLookupAndStore(t *testing.T) {
	// race detector 환경에서 동시 lookup / store / replace 가 안전한지 확인한다.
	p := newPodMap()
	const pidCount = 64

	var wg sync.WaitGroup
	for i := 0; i < pidCount; i++ {
		wg.Add(1)
		go func(pid uint32) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				p.store(pid, samplePod("ml", "p", "u"))
				_, _ = p.lookup(pid)
			}
		}(uint32(i))
	}

	// 동시에 replace 도 여러 번 호출해 lock 경합을 발생시킨다.
	for k := 0; k < 10; k++ {
		fresh := make(map[uint32]kube.PodIdentity, pidCount)
		for i := uint32(0); i < pidCount; i++ {
			fresh[i] = samplePod("ml", "p", "u")
		}
		p.replace(fresh)
	}

	wg.Wait()
}
