package cuda

import "sync"

// deviceMap 은 host PID 를 GPU UUID 로 매핑하는 atomic-replace 캐시다.
// runDeviceMapRefresher 가 NVML RunningProcesses 결과로 주기적으로 fresh map 을 통째 교체하고,
// reader 루프는 이벤트 도착 시점에 lookup 한다. 한 폴링 사이클의 결과를 통째로 swap 해서
// "key 한 개씩 갱신" 패턴이 만드는 short-lived 부분 일관성 윈도우를 회피한다.
//
// race-free 보장: replace / lookup 은 mu 보호 하에서만 pidToUUID 슬롯에 접근한다.
// 호출자가 단일 reader / 단일 refresher 라도 ctx 종료 등 비정상 경로에서 동시 접근이 일어날
// 수 있어 RWMutex 로 감싼다.
type deviceMap struct {
	mu        sync.RWMutex
	pidToUUID map[uint32]string
}

func newDeviceMap() *deviceMap {
	return &deviceMap{pidToUUID: make(map[uint32]string)}
}

// replace 는 fresh map 으로 통째 교체한다. 호출자는 fresh 맵을 더 이상 변형해서는 안 된다 —
// 본 구조체가 그 슬롯을 배타적으로 소유한다.
func (d *deviceMap) replace(fresh map[uint32]string) {
	d.mu.Lock()
	d.pidToUUID = fresh
	d.mu.Unlock()
}

// lookup 은 매칭되는 GPU UUID 를 반환한다. PID 가 어떤 GPU 에도 등록되지 않았으면 빈 문자열을 반환하고,
// 호출자(metrics.RecordCudaEvent) 는 이를 "unknown" 폴백으로 처리한다.
func (d *deviceMap) lookup(pid uint32) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.pidToUUID[pid]
}
