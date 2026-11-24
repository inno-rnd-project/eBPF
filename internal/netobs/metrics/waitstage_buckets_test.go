package metrics

import (
	"strconv"
	"testing"
)

// TestStageLatencyBucketsCarryWaitStageTailBounds는 netobs_stage_latency_seconds의 버킷에 대기 stage 꼬리
// 비율 rule이 le 라벨로 하드코딩한 경계가 그대로 있는지 가드한다. rule(deploy/gpuobs/base/
// prometheus-rule-network.yaml의 node:netobs_wait_stage_tail:rate5m)은 le="0.524288"과 le="1.048576"
// 버킷을 직접 고르므로, 버킷 구성이 바뀌어 이 경계가 사라지면 시리즈가 비어 NetObsHighWaitStageTailRatio가
// 오류 없이 죽는다. 버킷을 바꿀 때는 rule의 경계와 promtool 테스트를 함께 옮긴다. le 라벨은 Prometheus가
// 경계 float를 최단 표기로 적은 문자열이라 같은 방식으로 비교한다.
func TestStageLatencyBucketsCarryWaitStageTailBounds(t *testing.T) {
	have := map[string]bool{}
	for _, b := range stageLatencyBuckets {
		have[strconv.FormatFloat(b, 'g', -1, 64)] = true
	}
	for _, le := range []string{"0.524288", "1.048576"} {
		if !have[le] {
			t.Errorf("netobs_stage_latency_seconds에 le=%q 버킷이 없음: 대기 stage 꼬리 비율 rule의 경계와 어긋난다", le)
		}
	}
}
