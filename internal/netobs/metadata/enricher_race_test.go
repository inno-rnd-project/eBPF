package metadata

import (
	"sync"
	"testing"
	"time"

	"netobs/internal/kube"
)

// TestEnricher_RWMutexPromoteIdempotent 는 #107 audit 의 회귀 가드 다. lookupFlow 의 promote 경로 에서
// RUnlock 과 Lock 재획득 사이 TOCTOU 윈도우 안 의 다중 goroutine 동시 promote 가 idempotent 가드 (`if _,
// already := e.flowCurrent[cookie]; !already`) 로 정합 흡수 되는지 multi-goroutine reproducer 로 검증한다.
// go test -race 와 결합 해 본 테스트가 race detector 가 잡을 수 있는 표면적을 영구 확보 한다.
func TestEnricher_RWMutexPromoteIdempotent(t *testing.T) {
	e := NewEnricher(nil)

	// previous 맵에 1 entry 시드. lookupFlow 가 previous hit 후 current 로 promote 시도 하는 경로 진입.
	const cookie uint64 = 0xDEADBEEF
	seed := flowCacheEntry{
		Src: kube.PodIdentity{Namespace: "ns-a", PodName: "src", PodUID: "uid-src"},
		Dst: kube.PodIdentity{Namespace: "ns-b", PodName: "dst", PodUID: "uid-dst"},
	}
	e.mu.Lock()
	e.flowPrevious[cookie] = seed
	e.mu.Unlock()

	const workers = 32
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				entry, ok := e.lookupFlow(cookie)
				if !ok {
					t.Errorf("lookupFlow miss after seed (race 결함)")
					return
				}
				if entry.Src.PodUID != "uid-src" || entry.Dst.PodUID != "uid-dst" {
					t.Errorf("lookupFlow 반환값 불일치: src=%q dst=%q", entry.Src.PodUID, entry.Dst.PodUID)
					return
				}
			}
		}()
	}
	wg.Wait()

	// 모든 goroutine 종료 후 current 에 cookie 가 정확히 1 entry 로 promote 되어 있어야 idempotent invariant.
	e.mu.RLock()
	defer e.mu.RUnlock()
	if got, ok := e.flowCurrent[cookie]; !ok {
		t.Fatal("flowCurrent 에 cookie 가 promote 되지 않음")
	} else if got.Src.PodUID != "uid-src" {
		t.Errorf("promote 결과 Src 불일치: %q", got.Src.PodUID)
	}
}

// TestEnricher_SetCgroupScannerRace 는 #358 회귀 가드 다. netobs-agent 기동 시 메트릭 서버 goroutine
// 이 SetCgroupScanner 주입보다 먼저 시작해, 그 사이 scrape 가 ResolveCgroup → lookupCgroupHint 로
// e.cgroupScanner 를 읽으면서 주입 쓰기와 경합하던 기동 윈도우 를 재현한다. Set 과 Resolve 를 동시 진입
// 시켜 go test -race 가 무동기 접근 을 잡을 수 있는 표면적 을 영구 확보 한다.
func TestEnricher_SetCgroupScannerRace(t *testing.T) {
	e := NewEnricher(nil)
	scanner := NewCgroupScanner(nil, "node-a", "/sys/fs/cgroup")

	const workers = 16
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(workers * 2)
	// Writer goroutine: 기동 윈도우 의 주입 쓰기 를 반복 자극.
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				e.SetCgroupScanner(scanner)
			}
		}()
	}
	// Reader goroutine: scrape 경로 의 ResolveCgroup 동시 진입. kr 이 nil 이라 Lookup 은 항상 miss 지만
	// e.cgroupScanner 읽기 자체 가 write 와 경합 하는 race window 를 자극 한다.
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				_, _ = e.ResolveCgroup(uint64(0x2000 + j))
			}
		}()
	}
	wg.Wait()
}

// TestEnricher_ConcurrentMultiOpRace 는 lookupFlow read 경로 와 rememberFlow write / rotate 경로 가
// multi-goroutine 동시 진입 시 race detector 위반 없이 흐르는지 검증. flowCurrent 와 flowPrevious 의
// 일관된 RWMutex 보호 영구 가드. read-only 시나리오 만 으로는 rotate 경로 의 동시 쓰기 race window 가
// 자극 되지 않아 reader 와 writer goroutine 을 동시 진입 시키는 패턴 으로 보강.
func TestEnricher_ConcurrentMultiOpRace(t *testing.T) {
	e := NewEnricher(nil)

	// 다양한 cookie 의 entry 를 previous / current 에 분산 시드.
	for i := 0; i < 16; i++ {
		cookie := uint64(0x1000 + i)
		entry := flowCacheEntry{
			Src: kube.PodIdentity{PodUID: "uid-" + string(rune('a'+i))},
		}
		e.mu.Lock()
		if i%2 == 0 {
			e.flowPrevious[cookie] = entry
		} else {
			e.flowCurrent[cookie] = entry
		}
		e.mu.Unlock()
	}

	const workers = 16
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(workers * 2)
	// Reader goroutine N 개: lookupFlow read 경로 동시 진입.
	for i := 0; i < workers; i++ {
		gid := i
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				cookie := uint64(0x1000 + ((gid + j) % 16))
				_, _ = e.lookupFlow(cookie)
			}
		}()
	}
	// Writer goroutine N 개: rememberFlow 호출로 flowCurrent write 와 maybeRotateFlowsLocked 진입.
	// reader 와 동시 발생 해 RWMutex 의 Lock / RLock 경합 표면적 확보. flowMaxCurrent 초과 시 rotate
	// 도 함께 자극 되어 flowCurrent / flowPrevious 통째 교체 와 lookup 의 race-free 검증.
	for i := 0; i < workers; i++ {
		gid := i
		go func() {
			defer wg.Done()
			now := time.Now()
			for j := 0; j < iterations; j++ {
				cookie := uint64(0x1000 + ((gid + j) % 16))
				e.rememberFlow(cookie,
					kube.PodIdentity{PodUID: "uid-write-src"},
					kube.PodIdentity{PodUID: "uid-write-dst"},
					now)
			}
		}()
	}
	wg.Wait()
}
