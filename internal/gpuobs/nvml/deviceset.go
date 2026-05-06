package nvml

import (
	"log"
	"sync"
)

// DeviceSet 는 NVML device 목록의 hot-plug 동적 추적을 제공하는 thread-safe 컨테이너다.
//
// 호출자는 매 polling 사이클에 Sync 한 번 → Snapshot 으로 device 슬라이스 복사본을 받아 순회한다.
// Sync 는 NVML 의 현재 (index → UUID) 매핑을 다시 읽어, 본 셋에 없는 신규 UUID 는 Device handle 을
// Open 해 추가하고, 사라진 UUID 는 Close 해 제거한다. 안정 식별자는 UUID 이며 index 는 hot-remove 시
// remaining device 들이 재배치될 수 있어 직접 키로 쓰지 않는다.
//
// 단일 호출자 (예: collector pollOnce 또는 cuda refresher) 가 주기적으로 Sync 한다고 가정하지만,
// Snapshot / Close 가 다른 goroutine 에서 호출되어도 mutex 로 보호된다.
type DeviceSet struct {
	nv NVML

	mu     sync.RWMutex
	byUUID map[string]Device
}

// NewDeviceSet 는 빈 셋을 만든다. 첫 Sync 호출 시 NVML 의 현재 device 들이 등록된다.
func NewDeviceSet(nv NVML) *DeviceSet {
	return &DeviceSet{
		nv:     nv,
		byUUID: make(map[string]Device),
	}
}

// Sync 는 NVML 의 현재 device 목록과 본 셋의 상태를 일치시킨다.
//
//   - 신규 UUID: nv.Device(index) 로 handle Open 후 셋에 추가.
//   - 기존 UUID: 그대로 유지 (handle 재사용).
//   - 사라진 UUID: handle.Close 후 셋에서 제거.
//
// DeviceCount / DeviceUUID 호출이 실패하면 에러를 반환하지만, 개별 index 의 UUID 조회 / Open 실패는
// 해당 device 만 건너뛰고 다른 device 와 cleanup 은 정상 진행한다 — 부분 실패가 전체 sync 를 막지 않는다.
func (s *DeviceSet) Sync() error {
	count, err := s.nv.DeviceCount()
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, count)
	for i := uint(0); i < count; i++ {
		uuid, err := s.nv.DeviceUUID(i)
		if err != nil {
			log.Printf("nvml deviceset: uuid idx=%d: %v", i, err)
			continue
		}
		seen[uuid] = struct{}{}

		s.mu.RLock()
		_, exists := s.byUUID[uuid]
		s.mu.RUnlock()
		if exists {
			continue
		}

		dev, err := s.nv.Device(i)
		if err != nil {
			log.Printf("nvml deviceset: open idx=%d uuid=%s: %v", i, uuid, err)
			continue
		}
		s.mu.Lock()
		// race: 두 goroutine 이 동시에 같은 신규 UUID 를 Open 한 경우 마지막 승자만 셋에 남고 직전 핸들은 leak 된다.
		// 본 구조체는 단일 sync 호출자 가정이므로 정상 경로엔 도달하지 않지만, 안전망으로 중복 등록을 방어한다.
		if existing, dup := s.byUUID[uuid]; dup {
			s.mu.Unlock()
			_ = dev.Close()
			_ = existing
			continue
		}
		s.byUUID[uuid] = dev
		s.mu.Unlock()
	}

	// 사라진 UUID 정리. iteration 중 변형을 피하기 위해 먼저 후보를 모은다.
	s.mu.RLock()
	stale := make([]string, 0, len(s.byUUID))
	for uuid := range s.byUUID {
		if _, alive := seen[uuid]; !alive {
			stale = append(stale, uuid)
		}
	}
	s.mu.RUnlock()

	for _, uuid := range stale {
		s.mu.Lock()
		dev, ok := s.byUUID[uuid]
		if !ok {
			s.mu.Unlock()
			continue
		}
		delete(s.byUUID, uuid)
		s.mu.Unlock()
		if err := dev.Close(); err != nil {
			log.Printf("nvml deviceset: close uuid=%s: %v", uuid, err)
		}
	}

	return nil
}

// Snapshot 은 현재 알려진 모든 device 의 슬라이스 복사본을 반환한다. caller 가 자유롭게 보유 / 순회해도
// 안전하지만, 다음 Sync 호출 후에는 일부 Device 가 Close 되어 메서드 호출 시 에러 또는 panic 가능성이
// 있으므로 한 폴링 사이클 안에서만 사용한다고 가정한다.
//
// 슬라이스 순서는 map iteration 에 의존해 비결정적이다. 호출자가 정렬 의존성이 있다면 별도 sort 한다.
func (s *DeviceSet) Snapshot() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Device, 0, len(s.byUUID))
	for _, dev := range s.byUUID {
		out = append(out, dev)
	}
	return out
}

// Len 은 현재 셋에 등록된 device 수를 반환한다. 진단 / 메트릭 용.
func (s *DeviceSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byUUID)
}

// Close 는 모든 device handle 을 닫고 셋을 비운다. 에이전트 종료 시 1회 호출되며, 첫 번째로 발생한
// Close 에러를 반환한다 (이후 에러는 로그로만 남기고 close 시도는 모든 device 에 대해 끝까지 진행한다).
func (s *DeviceSet) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for uuid, dev := range s.byUUID {
		if err := dev.Close(); err != nil {
			log.Printf("nvml deviceset: close uuid=%s: %v", uuid, err)
			if firstErr == nil {
				firstErr = err
			}
		}
		delete(s.byUUID, uuid)
	}
	return firstErr
}
