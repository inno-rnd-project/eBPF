package metrics

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/netobs/types"
)

// podInstanceSeriesCount 는 pod-instance 메트릭 4종의 현재 시리즈 수 합계다. histogram 은 CollectAndCount
// 가 시리즈 (bucket 포함) 수를 세므로 정리 전후 수렴 판정에 그대로 쓴다.
func podInstanceSeriesCount() int {
	return testutil.CollectAndCount(podStageEventsLabeled) +
		testutil.CollectAndCount(podStageLatencyLabeled) +
		testutil.CollectAndCount(sendPathFullLatencySeconds) +
		testutil.CollectAndCount(sendPathSegmentCountTotal)
}

// emitPodInstanceEvent 는 지정 UID 의 pod-instance 메트릭 4종을 모두 emit 한다. sendmsg_ret +
// SegmentCount>0 조합이 4종 전부를 통과시키는 유일한 경로다.
func emitPodInstanceEvent(uid string) {
	ev := sampleEvent(podID("ns-a", "pod-"+uid, uid), podID("ns-dst", "dst-pod", "uid-dst"), types.StageSendmsgRet, "sendmsg_ret")
	ev.Raw.SegmentCount = 3
	ev.Raw.FullLatencyNs = 1_000_000
	Record(ev)
}

// TestCleanupStalePodInstanceSeries_RemovesDeadUID 는 활성 셋에서 사라진 UID 의 시리즈 4종이 전부
// 회수되고 활성 UID 의 시리즈는 유지되는지 검증한다 (#407).
func TestCleanupStalePodInstanceSeries_RemovesDeadUID(t *testing.T) {
	resetMetrics()
	emitPodInstanceEvent("uid-dead")
	emitPodInstanceEvent("uid-live")
	perUID := podInstanceSeriesCount() / 2
	if perUID == 0 {
		t.Fatal("emit 이 시리즈를 만들지 않음 (테스트 전제 실패)")
	}

	deleted := CleanupStalePodInstanceSeries(map[string]struct{}{"uid-live": {}})
	if deleted == 0 {
		t.Error("deleted=0 want >0")
	}
	if got := podInstanceSeriesCount(); got != perUID {
		t.Errorf("정리 후 시리즈=%d want %d (uid-live 분만 잔존)", got, perUID)
	}

	// 활성 UID 만 남은 상태에서 재정리는 no-op 이다.
	if deleted := CleanupStalePodInstanceSeries(map[string]struct{}{"uid-live": {}}); deleted != 0 {
		t.Errorf("재정리 deleted=%d want 0", deleted)
	}
}

// TestCleanupStalePodInstanceSeries_UnknownUIDPreserved 는 UID 부재 대치값 "unknown" 시리즈가 수명
// 관리 대상에서 제외되는지 검증한다. unknown 은 특정 pod 수명에 귀속되지 않아 삭제하면 매 주기
// counter reset 이 반복된다.
func TestCleanupStalePodInstanceSeries_UnknownUIDPreserved(t *testing.T) {
	resetMetrics()
	emitPodInstanceEvent("") // podUIDLabel 이 "unknown" 으로 대치
	before := podInstanceSeriesCount()
	if before == 0 {
		t.Fatal("emit 이 시리즈를 만들지 않음 (테스트 전제 실패)")
	}
	if deleted := CleanupStalePodInstanceSeries(map[string]struct{}{}); deleted != 0 {
		t.Errorf("deleted=%d want 0 (unknown 은 정리 대상 아님)", deleted)
	}
	if got := podInstanceSeriesCount(); got != before {
		t.Errorf("시리즈=%d want %d (unknown 보존)", got, before)
	}
}

// TestCleanupStalePodInstanceSeries_RestartChurnConverges 는 이슈 #407 의 핵심 시나리오다. pod 재시작
// (새 UID) 이 반복되어도 매 주기 정리가 돌면 시리즈 수가 활성 pod 1개 분으로 수렴하고 단조 누적되지
// 않는다. straggler (삭제 직후 재유입) 도 다음 주기에 재정리된다.
func TestCleanupStalePodInstanceSeries_RestartChurnConverges(t *testing.T) {
	resetMetrics()
	emitPodInstanceEvent("uid-gen-0")
	perUID := podInstanceSeriesCount()

	for gen := 1; gen <= 5; gen++ {
		uid := fmt.Sprintf("uid-gen-%d", gen)
		emitPodInstanceEvent(uid)
		CleanupStalePodInstanceSeries(map[string]struct{}{uid: {}})
		if got := podInstanceSeriesCount(); got != perUID {
			t.Fatalf("gen=%d 시리즈=%d want %d (재시작 churn 에도 수렴)", gen, got, perUID)
		}
	}

	// straggler: 삭제된 uid-gen-1 의 이벤트가 뒤늦게 도착해 시리즈가 재생성되어도 다음 주기에
	// 다시 잡힌다.
	emitPodInstanceEvent("uid-gen-1")
	if got := podInstanceSeriesCount(); got != perUID*2 {
		t.Fatalf("straggler 재생성 시리즈=%d want %d", got, perUID*2)
	}
	CleanupStalePodInstanceSeries(map[string]struct{}{"uid-gen-5": {}})
	if got := podInstanceSeriesCount(); got != perUID {
		t.Errorf("straggler 재정리 후 시리즈=%d want %d (자기치유)", got, perUID)
	}
}
