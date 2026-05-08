package cuda

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// visDevMap 은 PID 별 NVIDIA_VISIBLE_DEVICES 환경변수를 해석한 결과인 "컨테이너 CUDA driver
// ordinal → 호스트 GPU UUID" 매핑을 캐시한다. 컨테이너의 CUDA driver 가 device 0, 1, 2 로
// 보는 순서가 NVIDIA_VISIBLE_DEVICES 의 항목 순서와 일치한다는 nvidia-container-runtime 동작
// 가정에 기반한다.
//
// 본 자료구조는 BPF 가 capture 하는 device ordinal (#33 후속 commit) 을 에이전트가 발행하는
// 호스트 NVML UUID 라벨로 변환하는 데 사용된다. dispatch hot path 가 매 이벤트마다
// /proc/<pid>/environ 을 다시 read + parse 하지 않도록 podMap 과 동일한 atomic-replace + lazy
// fill 하이브리드 패턴을 쓴다.
//
// race-free 보장: lookup 은 RLock, replace / store 는 Lock 으로 직렬화한다. dispatch 가 hot
// path 에서 RLock 만 거치도록 store 분기는 cache miss 일 때만 실행된다.
type visDevMap struct {
	mu          sync.RWMutex
	pidOrdinals map[uint32][]string
}

func newVisDevMap() *visDevMap {
	return &visDevMap{pidOrdinals: make(map[uint32][]string)}
}

// replace 는 fresh map 으로 통째 교체한다. 호출자는 fresh 맵을 더 이상 변형해서는 안 된다.
// NVML refresh 사이클이 active PID 셋에 대해 한 번씩 NVIDIA_VISIBLE_DEVICES 를 파싱해 만든
// fresh 맵을 본 함수에 넘긴다. 종료된 PID 는 자연스럽게 빠진다.
func (v *visDevMap) replace(fresh map[uint32][]string) {
	v.mu.Lock()
	v.pidOrdinals = fresh
	v.mu.Unlock()
}

// lookup 은 PID 의 ordinal-to-UUID 슬라이스와 존재 여부를 반환한다. ok=false 는 cache miss
// 를 의미하며, 호출자가 lazy fill 을 위해 readVisibleDevices 와 hostResolver 를 거쳐 store 로
// 적재한다. nil 슬라이스도 ok=true 로 반환되도록 의도되어 있어 (NVIDIA_VISIBLE_DEVICES 가
// 비어 있거나 void 인 경우의 negative result 캐시), 동일 PID 에 대해 환경변수 read + parse
// 가 한 번만 일어나도록 보장한다.
func (v *visDevMap) lookup(pid uint32) ([]string, bool) {
	v.mu.RLock()
	ords, ok := v.pidOrdinals[pid]
	v.mu.RUnlock()
	return ords, ok
}

// store 는 단일 (pid, ordinals) 를 캐시에 적재한다. dispatch 가 cache miss 후 즉석 적재할 때
// 호출하며 NVML refresh 사이의 신규 PID 가 두 번째 이벤트부터 hit 경로로 들어가게 한다.
// 동일 PID 의 후속 store 가 직전 값을 덮어써도 NVIDIA_VISIBLE_DEVICES 는 PID 의 수명 동안
// 변하지 않으므로 정합성 문제가 없다.
func (v *visDevMap) store(pid uint32, ordinals []string) {
	v.mu.Lock()
	v.pidOrdinals[pid] = ordinals
	v.mu.Unlock()
}

// resolve 는 PID 의 ordinal 슬라이스에서 ordinal 인덱스에 해당하는 host UUID 를 반환한다.
// 매핑이 없거나 ordinal 이 슬라이스 범위를 벗어나면 빈 문자열을 돌려주고, 호출자 측에서
// metrics 의 "unknown" 폴백 또는 기존 devmap.lookup 폴백으로 분기한다.
func (v *visDevMap) resolve(pid uint32, ordinal int) string {
	ords, ok := v.lookup(pid)
	if !ok || ordinal < 0 || ordinal >= len(ords) {
		return ""
	}
	return ords[ordinal]
}

// readNVIDIAVisibleDevices 는 /proc/<pid>/environ 을 읽어 NVIDIA_VISIBLE_DEVICES 환경변수의
// 값을 추출한다. environ 파일은 NUL byte 로 구분된 KEY=VALUE 셋이라 bytes 패키지로 byte-slice
// 상태에서 직접 분해해 string(data) 의 전체 복사 alloc 을 피한다. 매칭된 항목의 값 부분만
// string 으로 변환해 반환 alloc 을 최소화한다. 파일 자체가 없거나 (PID 종료) 환경변수가 설정되지
// 않은 경우 빈 문자열을 반환해 호출자가 negative cache 로 처리할 수 있게 한다.
func readNVIDIAVisibleDevices(pid uint32) (string, error) {
	path := fmt.Sprintf("/proc/%d/environ", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	keyBytes := []byte("NVIDIA_VISIBLE_DEVICES=")
	for _, entry := range bytes.Split(data, []byte{0}) {
		if bytes.HasPrefix(entry, keyBytes) {
			return string(entry[len(keyBytes):]), nil
		}
	}
	return "", nil
}

// parseVisibleDevices 는 NVIDIA_VISIBLE_DEVICES 의 값을 호스트 GPU UUID 의 ordinal-인덱싱
// 슬라이스로 해석한다. 컨테이너의 CUDA driver 가 device 0, 1, 2 로 노출하는 순서가 본 슬라이스
// 인덱스와 정확히 같다고 가정한다 (nvidia-container-runtime 의 표준 동작).
//
// 입력 포맷:
//   - "all": 호스트의 모든 GPU 를 NVML index 순서대로 노출
//   - "none" / "void" / 빈 문자열: GPU 미노출 (반환 nil)
//   - "0,1,2": 콤마 구분 호스트 NVML index. hostUUIDByIndex 로 UUID 로 변환
//   - "GPU-uuid-1,GPU-uuid-2": 콤마 구분 UUID 그대로 사용
//   - 위 두 가지가 섞인 케이스: 항목별로 개별 해석
//
// 매핑 실패 항목 (모르는 index / UUID) 은 빈 문자열로 자리는 유지한다. ordinal 의 위치 의미가
// 깨지지 않도록 항목을 누락시키지 않는 게 중요하다.
func parseVisibleDevices(value string, hostUUIDByIndex map[int]string, hostUUIDSet map[string]struct{}) []string {
	value = strings.TrimSpace(value)
	switch value {
	case "", "void", "none":
		return nil
	case "all":
		// 컨테이너 CUDA driver 는 NVIDIA_VISIBLE_DEVICES=all 일 때 호스트의 가용 GPU 를 dense ordinal
		// (0, 1, ..., N-1) 로 인식한다. 호스트 NVML index 에 gap (예: MIG 분할 / hot-remove 후) 이
		// 있어도 컨테이너 ordinal 에는 그대로 전달되지 않으므로, NVML index 를 ASC 정렬해 dense
		// packing 한 결과를 반환한다. 콤마 구분 케이스 (아래 분기) 와 의미가 일관된다.
		indices := make([]int, 0, len(hostUUIDByIndex))
		for idx := range hostUUIDByIndex {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		result := make([]string, len(indices))
		for i, idx := range indices {
			result[i] = hostUUIDByIndex[idx]
		}
		return result
	}

	parts := strings.Split(value, ",")
	result := make([]string, len(parts))
	for i, raw := range parts {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		// UUID 직접 명시 케이스 우선 처리. NVIDIA UUID 는 보통 "GPU-..." prefix 라 한 번에 식별되지만,
		// hostUUIDSet 에 정확히 등록된 값만 신뢰해 unknown UUID 는 빈 문자열로 둔다.
		if _, known := hostUUIDSet[entry]; known {
			result[i] = entry
			continue
		}
		// index 케이스: 비-UUID 형식이면 정수로 파싱해 hostUUIDByIndex 에서 lookup
		if idx, err := strconv.Atoi(entry); err == nil {
			result[i] = hostUUIDByIndex[idx]
			continue
		}
		// 어느 분기에도 매칭 안 되면 빈 문자열로 자리만 보존 (디버깅 시 unknown 으로 식별 가능)
	}
	return result
}
