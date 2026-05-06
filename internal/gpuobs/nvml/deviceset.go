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

// indexUpdater 는 deviceImpl 이 NVML index 재배치를 반영할 수 있도록 노출하는 unexported 메서드 셋이다.
// DeviceSet 이 같은 nvml 패키지에서 deviceImpl 의 updateIndex 를 호출할 때 type assertion 으로 쓰인다.
// fake / 외부 Device 구현은 본 인터페이스를 만족하지 않으면 index 갱신을 단순히 skip 한다 — 본 fallback 은
// 운영 deviceImpl 에만 영향을 주고, 테스트 fake 가 메서드를 추가 구현하면 동일 동작이 검증된다.
type indexUpdater interface {
	updateIndex(uint)
}

// Sync 는 NVML 의 현재 device 목록과 본 셋의 상태를 일치시킨다.
//
//   - 신규 UUID: nv.Device(index) 로 handle Open 후 셋에 추가. dev.Info().UUID 가 기대 UUID 와
//     일치하지 않는 race (DeviceUUID(i) 와 Device(i) 사이 hot-remove + index 재배치) 케이스는 close + skip.
//   - 기존 UUID: handle 재사용. NVML index 가 바뀌었을 가능성에 대비해 indexUpdater 를 통해 currentIdx 를 갱신.
//   - 사라진 UUID: handle.Close 후 셋에서 제거.
//
// DeviceCount 실패는 즉시 에러를 반환한다. 개별 index 의 UUID 조회 / Device Open / Info 조회 / UUID
// mismatch 등 부분 실패가 발생한 사이클에서는 stale cleanup 단계 자체를 건너뛴다. 한 cycle 의 일시
// 실패가 정상 device 의 handle close → 다음 cycle 의 재 Open 으로 이어지면서 GPM init 등 device-scope
// 자원이 churn 하는 것을 방지한다.
func (s *DeviceSet) Sync() error {
	count, err := s.nv.DeviceCount()
	if err != nil {
		return err
	}

	seen := make(map[string]struct{}, count)
	hasError := false
	for i := uint(0); i < count; i++ {
		uuid, err := s.nv.DeviceUUID(i)
		if err != nil {
			log.Printf("nvml deviceset: uuid idx=%d: %v", i, err)
			hasError = true
			continue
		}
		seen[uuid] = struct{}{}

		s.mu.RLock()
		existing, exists := s.byUUID[uuid]
		s.mu.RUnlock()
		if exists {
			// 기존 UUID 의 NVML index 가 hot-remove 후 재배치되었을 수 있으므로 currentIdx 를 갱신한다.
			// fake 등 indexUpdater 미구현 Device 는 자연 skip 되어 호출 부담이 0 이다.
			if updater, ok := existing.(indexUpdater); ok {
				updater.updateIndex(i)
			}
			continue
		}

		dev, err := s.nv.Device(i)
		if err != nil {
			log.Printf("nvml deviceset: open idx=%d uuid=%s: %v", i, uuid, err)
			hasError = true
			continue
		}
		// race 가드: DeviceUUID(i) 와 Device(i) 호출 사이에 hot-remove + index 재배치가 일어나
		// dev 의 실제 UUID 가 기대 UUID 와 다를 수 있다. mismatch 시 dev 를 close 하고 skip 해
		// byUUID 에 잘못된 매핑이 등록되지 않게 한다.
		if info, infoErr := dev.Info(); infoErr != nil {
			log.Printf("nvml deviceset: info idx=%d uuid=%s: %v", i, uuid, infoErr)
			_ = dev.Close()
			hasError = true
			continue
		} else if info.UUID != uuid {
			log.Printf("nvml deviceset: uuid mismatch idx=%d expected=%s actual=%s; skipping", i, uuid, info.UUID)
			_ = dev.Close()
			hasError = true
			continue
		}
		s.mu.Lock()
		// 동일 UUID 가 한 sync 안에서 두 번 Open 되는 경로는 단일 sync 호출자 가정 하에 도달하지 않지만,
		// 안전망으로 중복 등록을 방어한다. 이 분기에 들어오면 새로 연 dev 를 Close 하고 기존 핸들을 유지하므로
		// 핸들이 leak 되지는 않으며, 발생 가능한 오버헤드는 새 dev 의 Open/Close 비용 한 번뿐이다.
		if _, dup := s.byUUID[uuid]; dup {
			s.mu.Unlock()
			_ = dev.Close()
			continue
		}
		s.byUUID[uuid] = dev
		s.mu.Unlock()
	}

	// 한 cycle 안에서 부분 실패가 한 건이라도 발생했다면 stale cleanup 을 건너뛴다.
	// 다음 sync 에서 재시도되어 자연 회복되며, 정상 device 가 일시적 NVML transient error 로
	// close 되어 GPM init 등 device-scope 자원이 churn 하는 것을 막는다.
	if hasError {
		return nil
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
