// Package metrics 의 podcleanup.go 는 #407 의 pod-instance 메트릭 stale 시리즈 정리다. src_pod_uid
// 라벨을 갖는 4종 (pod_stage_events_labeled, pod_stage_latency_labeled, send_path_full_latency,
// send_path_segment_count) 은 pod 재시작마다 새 UID 시리즈가 생기는데 정리 호출이 전무해 에이전트
// 프로세스 수명 동안 단조 누적됐다 (histogram 은 라벨 조합 1개당 23 시리즈). gpuobs 의 diff 기반
// cleanup (RecordPodSnapshot) 과 같은 부류의 해법이나, 이벤트 드리븐 emit 특성상 스냅샷 간 diff
// 대신 emit 시점 UID 추적 + 활성 셋 대조를 쓴다. 삭제 직후 straggler 이벤트가 시리즈를 재생성해도
// emit 추적에 다시 잡혀 다음 주기에 재정리되는 자기치유 구조다.
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	emittedPodUIDsMu sync.RWMutex
	// emittedPodUIDs 는 pod-instance 메트릭 4종이 emit 한 src_pod_uid 집합이다. Record 의 hot path
	// 에서 RLock 존재 확인 후 신규 UID 만 Lock 삽입하므로 정상 상태 (기존 UID 재방문) 비용은 RLock
	// 1회다.
	emittedPodUIDs = make(map[string]struct{})
)

// trackPodUID 는 pod-instance 메트릭 emit 시점의 src_pod_uid 를 정리 대상 후보로 등록한다. "unknown"
// (UID 부재 대치값) 은 특정 pod 수명에 귀속되지 않는 시리즈라 수명 관리 대상에서 제외한다.
func trackPodUID(uid string) {
	if uid == "unknown" {
		return
	}
	emittedPodUIDsMu.RLock()
	_, ok := emittedPodUIDs[uid]
	emittedPodUIDsMu.RUnlock()
	if ok {
		return
	}
	emittedPodUIDsMu.Lock()
	emittedPodUIDs[uid] = struct{}{}
	emittedPodUIDsMu.Unlock()
}

// CleanupStalePodInstanceSeries 는 emit 이력이 있는 UID 중 activeUIDs 에 없는 것의 pod-instance
// 시리즈 4종을 DeletePartialMatch 로 회수하고 삭제한 시리즈 수를 반환한다 (#407). activeUIDs 는
// informer 의 노드 pod 스냅샷 (kube.Resolver.PodsOnNode) 에서 온 현재 활성 UID 셋이며, 호출 주기는
// main 이 MetadataRefresh 에 편승시킨다. 삭제된 UID 는 추적 집합에서도 제거되어 집합 크기가 활성
// pod 수준으로 수렴한다.
func CleanupStalePodInstanceSeries(activeUIDs map[string]struct{}) int {
	emittedPodUIDsMu.Lock()
	stale := make([]string, 0)
	for uid := range emittedPodUIDs {
		if _, ok := activeUIDs[uid]; !ok {
			stale = append(stale, uid)
			delete(emittedPodUIDs, uid)
		}
	}
	emittedPodUIDsMu.Unlock()

	deleted := 0
	for _, uid := range stale {
		match := prometheus.Labels{"src_pod_uid": uid}
		deleted += podStageEventsLabeled.DeletePartialMatch(match)
		deleted += podStageLatencyLabeled.DeletePartialMatch(match)
		deleted += sendPathFullLatencySeconds.DeletePartialMatch(match)
		deleted += sendPathSegmentCountTotal.DeletePartialMatch(match)
	}
	return deleted
}
