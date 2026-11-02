package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/types"
)

// -------------------------------------------------------------------
// podMetricsEnabled flag 동작
// -------------------------------------------------------------------

func TestPodMetricsEnabledFlag(t *testing.T) {
	// 다른 테스트와 라벨 충돌을 막기 위해 고유한 라벨값 사용
	ev := types.EnrichedEvent{
		Raw:          types.Event{Stage: types.StageSendmsgRet, LatencyUs: 100},
		Stage:        types.StageName(types.StageSendmsgRet),
		Src: types.PodIdentity{
			IdentityClass: types.IdentityClassPod,
			Namespace:     "flag-test-ns",
			PodName:       "flag-test-pod",
			PodUID:        "flag-test-uid",
		},
		ObservedNode: "flag-test-node",
		TrafficScope: "pod_to_pod",
		Direction:    "egress",
	}

	podLabels := prometheus.Labels{
		"stage":        types.StageName(types.StageSendmsgRet),
		"node":         "flag-test-node",
		"src_namespace": "flag-test-ns",
		"src_pod":       "flag-test-pod",
		"src_pod_uid":   "flag-test-uid",
		"traffic_scope": "pod_to_pod",
		"direction":     "egress",
	}

	t.Run("enabled=true: pod 메트릭 카운터 증가", func(t *testing.T) {
		SetPodMetricsEnabled(true)
		before := testutil.ToFloat64(podStageEventsLabeled.With(podLabels))
		Record(ev)
		after := testutil.ToFloat64(podStageEventsLabeled.With(podLabels))
		if diff := after - before; diff != 1 {
			t.Errorf("podMetricsEnabled=true: 카운터 +1 기대, got +%v", diff)
		}
	})

	t.Run("enabled=false: pod 메트릭 카운터 증가 안 함", func(t *testing.T) {
		SetPodMetricsEnabled(false)
		before := testutil.ToFloat64(podStageEventsLabeled.With(podLabels))
		Record(ev)
		after := testutil.ToFloat64(podStageEventsLabeled.With(podLabels))
		if diff := after - before; diff != 0 {
			t.Errorf("podMetricsEnabled=false: 카운터 변화 없어야 함, got +%v", diff)
		}
	})

	// 원래 상태 복원
	t.Cleanup(func() { SetPodMetricsEnabled(true) })
}

// -------------------------------------------------------------------
// Record: stage별 라벨 구성 검증
// -------------------------------------------------------------------

func TestRecordStageLabels(t *testing.T) {
	SetPodMetricsEnabled(true)

	t.Run("retrans stage: stageEventsLabeled + retransEventsLabeled 증가", func(t *testing.T) {
		ev := types.EnrichedEvent{
			Raw:   types.Event{Stage: types.StageRetrans},
			Stage: types.StageName(types.StageRetrans),
			Src: types.PodIdentity{
				IdentityClass: types.IdentityClassPod,
				Namespace:     "retrans-ns",
				WorkloadKind:  "Deployment",
				Workload:      "retrans-app",
			},
			ObservedNode: "retrans-node",
			TrafficScope: "cross_node",
			Direction:    "egress",
		}

		commonLabels := prometheus.Labels{
			"stage":        "retrans",
			"node":         "retrans-node",
			"src_namespace": "retrans-ns",
			"src_workload": "retrans-app",
			"traffic_scope": "cross_node",
			"direction":     "egress",
		}
		retransLabels := prometheus.Labels{
			"node":         "retrans-node",
			"src_namespace": "retrans-ns",
			"src_workload": "retrans-app",
			"traffic_scope": "cross_node",
			"direction":     "egress",
		}

		beforeStage := testutil.ToFloat64(stageEventsLabeled.With(commonLabels))
		beforeRetrans := testutil.ToFloat64(retransEventsLabeled.With(retransLabels))
		Record(ev)
		afterStage := testutil.ToFloat64(stageEventsLabeled.With(commonLabels))
		afterRetrans := testutil.ToFloat64(retransEventsLabeled.With(retransLabels))

		if d := afterStage - beforeStage; d != 1 {
			t.Errorf("stageEventsLabeled: +1 기대, got +%v", d)
		}
		if d := afterRetrans - beforeRetrans; d != 1 {
			t.Errorf("retransEventsLabeled: +1 기대, got +%v", d)
		}
	})

	t.Run("drop stage: dropEventsLabeled 증가", func(t *testing.T) {
		ev := types.EnrichedEvent{
			Raw:            types.Event{Stage: types.StageDrop, Reason: 1},
			Stage:          types.StageName(types.StageDrop),
			Src: types.PodIdentity{
				IdentityClass: types.IdentityClassPod,
				Namespace:     "drop-ns",
				WorkloadKind:  "Deployment",
				Workload:      "drop-app",
			},
			ObservedNode:   "drop-node",
			TrafficScope:   "to_external",
			Direction:      "egress",
			DropReasonName: "NO_SOCKET",
			DropCategory:   "socket",
		}

		dropLabels := prometheus.Labels{
			"node":          "drop-node",
			"src_namespace":  "drop-ns",
			"src_workload":  "drop-app",
			"traffic_scope": "to_external",
			"direction":     "egress",
			"drop_reason":   "NO_SOCKET",
			"drop_category": "socket",
		}

		before := testutil.ToFloat64(dropEventsLabeled.With(dropLabels))
		Record(ev)
		after := testutil.ToFloat64(dropEventsLabeled.With(dropLabels))
		if d := after - before; d != 1 {
			t.Errorf("dropEventsLabeled: +1 기대, got +%v", d)
		}
	})

	t.Run("sendmsg_ret stage: legacyLatencySeconds + stageLatencyLabeled 관측", func(t *testing.T) {
		ev := types.EnrichedEvent{
			Raw:   types.Event{Stage: types.StageSendmsgRet, LatencyUs: 1000},
			Stage: types.StageName(types.StageSendmsgRet),
			Src: types.PodIdentity{
				IdentityClass: types.IdentityClassPod,
				Namespace:     "lat-ns",
				WorkloadKind:  "Deployment",
				Workload:      "lat-app",
			},
			ObservedNode: "lat-node",
			TrafficScope: "same_node",
			Direction:    "egress",
		}

		// legacyLatencySeconds은 stage 라벨만 가짐
		legacyBefore := testutil.ToFloat64(legacyEventsTotal.WithLabelValues("sendmsg_ret"))
		Record(ev)
		legacyAfter := testutil.ToFloat64(legacyEventsTotal.WithLabelValues("sendmsg_ret"))
		if d := legacyAfter - legacyBefore; d != 1 {
			t.Errorf("legacyEventsTotal (sendmsg_ret): +1 기대, got +%v", d)
		}
	})

	t.Run("non-pod src: pod 메트릭 미방출", func(t *testing.T) {
		ev := types.EnrichedEvent{
			Raw:   types.Event{Stage: types.StageSendmsgRet, LatencyUs: 50},
			Stage: types.StageName(types.StageSendmsgRet),
			Src: types.PodIdentity{
				IdentityClass: types.IdentityClassExternal,
				PodIP:         "8.8.8.8",
			},
			ObservedNode: "nonpod-node",
			TrafficScope: "from_external",
			Direction:    "egress",
		}

		podLabels := prometheus.Labels{
			"stage":        "sendmsg_ret",
			"node":         "nonpod-node",
			"src_namespace": "external",
			"src_pod":       "unknown",
			"src_pod_uid":   "unknown",
			"traffic_scope": "from_external",
			"direction":     "egress",
		}

		// pod 메트릭은 IsPod()=false 이므로 증가하지 않아야 한다.
		before := testutil.ToFloat64(podStageEventsLabeled.With(podLabels))
		Record(ev)
		after := testutil.ToFloat64(podStageEventsLabeled.With(podLabels))
		if d := after - before; d != 0 {
			t.Errorf("non-pod src: pod 메트릭 변화 없어야 함, got +%v", d)
		}
	})
}
