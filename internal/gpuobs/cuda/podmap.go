package cuda

import (
	"sync"

	"netobs/internal/kube"
)

// podMap 은 host PID 를 kube.PodIdentity 로 매핑하는 atomic-replace 캐시다. cuda dispatch
// hot path 가 매 이벤트 호출하는 kube.Resolver.ResolvePID (/proc/<pid>/cgroup read + parse)
// 비용을 lookup 한 번으로 흡수해, 27 K Hz 의 PyTorch 워크로드에서 측정된 +737 mCPU overhead
// 를 줄이는 것이 목적이다 (docs/perf/cuda-uprobe-overhead.md 참고).
//
// 갱신 전략은 deviceMap 과 동일한 통째 교체 패턴 (replace) 에 dispatch 가 cache miss 시 즉석
// 적재하는 store 를 더한 하이브리드다. 통째 교체로 종료된 PID 가 자연 정리되고, store 로 NVML
// refresh 사이에 새로 등장한 PID 가 두 번째 이벤트부터 캐시 hit 경로로 진입한다.
//
// race-free 보장: lookup 은 RLock, replace / store 는 Lock 으로 직렬화한다. store 는 본질적으로
// 드문 경로라 Lock 을 받아도 hot path 평균 비용에 영향이 없다 (대다수 이벤트는 RLock 만 거친다).
type podMap struct {
	mu       sync.RWMutex
	pidToPod map[uint32]kube.PodIdentity
}

func newPodMap() *podMap {
	return &podMap{pidToPod: make(map[uint32]kube.PodIdentity)}
}

// replace 는 fresh map 으로 통째 교체한다. 호출자는 fresh 맵을 더 이상 변형해서는 안 된다.
// NVML refresh 사이클에서 active PID 셋에 대해 한 번씩 ResolvePID 를 호출해 만든 fresh 맵을
// 본 함수에 넘긴다. 종료된 PID 는 자연스럽게 빠지므로 cleanup 호출이 별도로 필요하지 않다.
func (p *podMap) replace(fresh map[uint32]kube.PodIdentity) {
	p.mu.Lock()
	p.pidToPod = fresh
	p.mu.Unlock()
}

// lookup 은 PID 에 매핑된 PodIdentity 와 존재 여부를 반환한다. ok=false 는 캐시 miss 를 의미하고
// 호출자가 lazy fill 을 위해 ResolvePID 를 호출한 뒤 store 로 적재하면 된다.
//
// PodIdentity 값 자체가 zero value 일 때 (resolver 가 비-Pod PID 로 판단한 경우) 도 ok=true 로
// 반환되도록 의도되어 있어, 호출자가 "캐시에 negative result 도 적재" 패턴으로 ResolvePID 를
// 한 번만 호출하게 만든다.
func (p *podMap) lookup(pid uint32) (kube.PodIdentity, bool) {
	p.mu.RLock()
	id, ok := p.pidToPod[pid]
	p.mu.RUnlock()
	return id, ok
}

// store 는 단일 (pid, id) 를 캐시에 적재한다. dispatch 가 cache miss 후 ResolvePID 결과를
// 즉석 적재할 때 호출하며, NVML refresh 사이의 신규 PID 가 두 번째 이벤트부터 hit 경로로
// 들어가게 한다. 같은 PID 에 대한 동시 store 는 lock 으로 직렬화되며, 동일 PID 의 후속 store
// 가 직전 값을 덮어써도 PodIdentity 는 같은 PID 에 대해 안정적이라 정합성 문제가 없다.
func (p *podMap) store(pid uint32, id kube.PodIdentity) {
	p.mu.Lock()
	p.pidToPod[pid] = id
	p.mu.Unlock()
}
