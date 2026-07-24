package metrics

import (
	"math"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"netobs/internal/kube"
	"netobs/internal/netobs/metadata"
	"netobs/internal/netobs/types"
)

// resetMetrics는 본 패키지 메트릭들의 패키지-레벨 상태를 테스트 사이에 격리한다. Counter / Histogram
// 은 register 한 번이면 누적이 이어지므로, 각 테스트가 자기만의 PedanticRegistry 에 다시 등록할 수
// 있도록 새 인스턴스로 갈아끼운다. 본 헬퍼가 test 전용이라 운영 코드는 영향받지 않는다.
func resetMetrics() {
	stageEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "netobs_stage_events_labeled_total"},
		[]string{"stage", "node", "src_namespace", "src_workload", "traffic_scope", "direction", "dst_namespace", "dst_workload"},
	)
	stageLatencyLabeled = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "netobs_stage_latency_labeled_seconds", Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20)},
		[]string{"stage", "node", "src_namespace", "src_workload", "traffic_scope", "direction", "dst_namespace", "dst_workload"},
	)
	dropEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "netobs_drop_events_labeled_total"},
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction", "drop_reason", "drop_category", "drop_stage", "dst_namespace", "dst_workload"},
	)
	retransEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "netobs_retrans_events_labeled_total"},
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction", "dst_namespace", "dst_workload"},
	)
	sockTeardownLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "netobs_sock_teardown_total"},
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction", "dst_namespace", "dst_workload"},
	)
	podStageEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "netobs_pod_stage_events_labeled_total"},
		[]string{"stage", "node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction", "dst_namespace", "dst_workload", "dst_pod_uid"},
	)
	podStageLatencyLabeled = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "netobs_pod_stage_latency_labeled_seconds", Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20)},
		[]string{"stage", "node", "src_namespace", "src_pod", "src_pod_uid", "traffic_scope", "direction", "dst_namespace", "dst_workload", "dst_pod_uid"},
	)
	legacyEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "netobs_events_total"}, []string{"stage"})
	legacyLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "netobs_stage_latency_seconds", Buckets: prometheus.ExponentialBuckets(1e-6, 2, 20)}, []string{"stage"})
	legacyDropTotal = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "netobs_drop_total"}, []string{"reason"})
	dstClassifierEmits = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "netobs_dst_classifier_emits_total"}, []string{"outcome"})
	nicCapacityBytesPerSec = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "netobs_node_nic_capacity_bytes_per_sec"}, []string{"node"})
	bpfProgramLoaded = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "netobs_bpf_program_loaded"}, []string{"symbol"})
	bpfRingbufDropsTotal = prometheus.NewCounter(prometheus.CounterOpts{Name: "netobs_bpf_ringbuf_drops_total"})
	bpfMapUtilizationRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "netobs_bpf_map_utilization_ratio"}, []string{"map"})
	informerSyncLagSeconds = prometheus.NewGauge(prometheus.GaugeOpts{Name: "netobs_informer_sync_lag_seconds"})
	dstClassifier.Store(nil)
	podMetricsEnabled.Store(true)
}

func sampleEvent(src, dst kube.PodIdentity, stage uint8, stageName string) types.EnrichedEvent {
	return types.EnrichedEvent{
		Raw:          types.Event{Stage: stage, LatencyUs: 1000},
		Stage:        stageName,
		Direction:    "egress",
		TrafficScope: "pod_to_pod",
		ObservedNode: "node-a",
		Src:          src,
		Dst:          dst,
	}
}

// labelValue는 PedanticRegistry 로 gather 한 결과에서 특정 메트릭의 특정 라벨 값을 끌어와 검증
// assertion 을 짧게 만든다. 한 시리즈만 emit 됐다고 가정한다.
func labelValue(t *testing.T, reg *prometheus.Registry, metricName, labelName string) string {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		if len(mf.Metric) != 1 {
			t.Fatalf("metric %q has %d series, want 1", metricName, len(mf.Metric))
		}
		for _, lp := range mf.Metric[0].Label {
			if lp.GetName() == labelName {
				return lp.GetValue()
			}
		}
		t.Fatalf("label %q missing from metric %q", labelName, metricName)
	}
	t.Fatalf("metric %q not found in registry", metricName)
	return ""
}

// TestRecordDstClassifierNilCollapsesDstLabels는 classifier 가 주입되지 않은 startup 직후 상태에서
// dst_namespace / dst_workload / dst_pod_uid 가 빈 문자열로 emit 되는지 검증한다. cardinality 가
// dst 라벨 도입 전과 동일하게 유지되는 default 운영 모드의 회귀 가드다.
func TestRecordDstClassifierNilCollapsesDstLabels(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(stageEventsLabeled)

	src := podID("ns-src", "src-pod", "uid-src")
	dst := podID("ns-dst", "dst-pod", "uid-dst")
	Record(sampleEvent(src, dst, types.StageSendmsgRet, "sendmsg_ret"))

	if v := labelValue(t, reg, "netobs_stage_events_labeled_total", "dst_namespace"); v != "" {
		t.Errorf("dst_namespace=%q want empty (classifier nil should collapse dst labels)", v)
	}
	if v := labelValue(t, reg, "netobs_stage_events_labeled_total", "dst_workload"); v != "" {
		t.Errorf("dst_workload=%q want empty", v)
	}
}

// TestRecordDstClassifierExternalMarker는 dst 가 클러스터 외부 IP 일 때 underscore prefix 합성
// 라벨 _external 로 emit 되는지 Record 전체 경로로 검증한다. classifier 단위 테스트에 더해
// metrics.Record 결선까지 가드한다.
func TestRecordDstClassifierExternalMarker(t *testing.T) {
	resetMetrics()
	SetDstClassifier(metadata.NewDstLabelClassifier(true, nil))
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(stageEventsLabeled)

	Record(sampleEvent(podID("ns-src", "src-pod", "uid-src"), externalID(), types.StageSendmsgRet, "sendmsg_ret"))

	if v := labelValue(t, reg, "netobs_stage_events_labeled_total", "dst_namespace"); v != "_external" {
		t.Errorf("dst_namespace=%q want _external", v)
	}
	if v := labelValue(t, reg, "netobs_stage_events_labeled_total", "dst_workload"); v != "_external" {
		t.Errorf("dst_workload=%q want _external", v)
	}
}

// TestRecordDstClassifierServiceLabel는 dst 가 Service IP 일 때 svc/<name> 표기로 emit 되는지
// 검증한다. backend Pod 으로 추가 해상하지 않는 정책의 회귀 가드다.
func TestRecordDstClassifierServiceLabel(t *testing.T) {
	resetMetrics()
	SetDstClassifier(metadata.NewDstLabelClassifier(true, nil))
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(stageEventsLabeled)

	Record(sampleEvent(podID("ns-src", "src-pod", "uid-src"), serviceID("default", "kubernetes"), types.StageSendmsgRet, "sendmsg_ret"))

	if v := labelValue(t, reg, "netobs_stage_events_labeled_total", "dst_namespace"); v != "default" {
		t.Errorf("dst_namespace=%q want default", v)
	}
	if v := labelValue(t, reg, "netobs_stage_events_labeled_total", "dst_workload"); !strings.HasPrefix(v, "svc/") {
		t.Errorf("dst_workload=%q want svc/* prefix", v)
	}
}

// TestRecordPodLevelDstUIDAllowListGate는 pod-level 메트릭의 dst_pod_uid 라벨이 allow-list 통과
// 시에만 채워지고 그 외에는 빈 값으로 emit 되는지 Record 전체 경로로 검증한다. cardinality 폭증
// 방지 가드의 positive / negative path 두 케이스를 한 함수에서 검사한다.
func TestRecordPodLevelDstUIDAllowListGate(t *testing.T) {
	t.Run("ns_in_allow_list_emits_uid", func(t *testing.T) {
		resetMetrics()
		SetPodMetricsEnabled(true)
		SetDstClassifier(metadata.NewDstLabelClassifier(true, []string{"ebpf-project"}))
		reg := prometheus.NewPedanticRegistry()
		reg.MustRegister(podStageEventsLabeled)

		src := podID("ns-src", "src-pod", "uid-src")
		dst := podID("ebpf-project", "dst-pod", "uid-dst-emit")
		Record(sampleEvent(src, dst, types.StageSendmsgRet, "sendmsg_ret"))

		if v := labelValue(t, reg, "netobs_pod_stage_events_labeled_total", "dst_pod_uid"); v != "uid-dst-emit" {
			t.Errorf("dst_pod_uid=%q want uid-dst-emit (namespace is in allow-list)", v)
		}
	})

	t.Run("ns_not_in_allow_list_emits_empty", func(t *testing.T) {
		resetMetrics()
		SetPodMetricsEnabled(true)
		SetDstClassifier(metadata.NewDstLabelClassifier(true, []string{"ebpf-project"}))
		reg := prometheus.NewPedanticRegistry()
		reg.MustRegister(podStageEventsLabeled)

		src := podID("ns-src", "src-pod", "uid-src")
		dst := podID("kube-system", "kube-proxy", "uid-secret")
		Record(sampleEvent(src, dst, types.StageSendmsgRet, "sendmsg_ret"))

		if v := labelValue(t, reg, "netobs_pod_stage_events_labeled_total", "dst_pod_uid"); v != "" {
			t.Errorf("dst_pod_uid=%q want empty (namespace not in allow-list)", v)
		}
	})
}

// TestSetNICCapacityBytesPerSec는 NIC capacity gauge 가 node 라벨과 함께 1회 Set 된 값을 그대로
// 노출하는지 검증한다. correlation pod:network_throughput_score:5m recording rule이 본 메트릭을
// 분모로 사용하므로 정적 값 노출이 핵심 invariant 다.
func TestSetNICCapacityBytesPerSec(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(nicCapacityBytesPerSec)

	SetNICCapacityBytesPerSec("node-a", 2.5e9)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var got float64
	var gotNode string
	for _, mf := range mfs {
		if mf.GetName() != "netobs_node_nic_capacity_bytes_per_sec" {
			continue
		}
		if len(mf.Metric) != 1 {
			t.Fatalf("series count=%d want 1", len(mf.Metric))
		}
		got = mf.Metric[0].GetGauge().GetValue()
		for _, lp := range mf.Metric[0].Label {
			if lp.GetName() == "node" {
				gotNode = lp.GetValue()
			}
		}
	}
	if got != 2.5e9 {
		t.Errorf("nic capacity=%v want 2.5e9", got)
	}
	if gotNode != "node-a" {
		t.Errorf("node label=%q want node-a", gotNode)
	}
}

// TestRecordSelfObserveCounter는 Record 가 classifier outcome 을 그대로
// netobs_dst_classifier_emits_total{outcome} 카운터에 위임하는지 검증한다. classifier 미설정 시점
// 의 disabled bucket, external dst 에서 external bucket 으로 각각 +1 됨을 두 케이스로 확인한다.
// 운영자가 cardinality bomb 징후를 본 카운터의 rate(pod_with_uid) 로 추적 가능한 기반이다.
func TestRecordSelfObserveCounter(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(dstClassifierEmits)

	// 1) classifier nil → outcome=disabled
	Record(sampleEvent(podID("ns-src", "src-pod", "uid-src"), podID("ns-dst", "dst-pod", "uid-dst"), types.StageSendmsgRet, "sendmsg_ret"))
	if v := labelValue(t, reg, "netobs_dst_classifier_emits_total", "outcome"); v != "disabled" {
		t.Errorf("outcome=%q want disabled (classifier nil)", v)
	}

	// 2) classifier 설정 후 external dst → outcome=external
	resetMetrics()
	SetDstClassifier(metadata.NewDstLabelClassifier(true, nil))
	reg = prometheus.NewPedanticRegistry()
	reg.MustRegister(dstClassifierEmits)
	Record(sampleEvent(podID("ns-src", "src-pod", "uid-src"), externalID(), types.StageSendmsgRet, "sendmsg_ret"))
	if v := labelValue(t, reg, "netobs_dst_classifier_emits_total", "outcome"); v != "external" {
		t.Errorf("outcome=%q want external", v)
	}
}

// TestRecord_SendPathStage4Decomposition 은 #82 의 신규 2 stage (tcp_write_xmit, tcp_transmit_skb)
// event 가 stageLatencyLabeled histogram 에 정확히 emit 되는지 검증한다. latency switch case 의
// 회귀 가드라 향후 stage enum 추가 시 본 테스트 갱신만으로 누락이 즉시 노출된다.
func TestRecord_SendPathStage4Decomposition(t *testing.T) {
	cases := []struct {
		stage     uint8
		stageName string
	}{
		{types.StageSendmsgRet, "sendmsg_ret"},
		{types.StageTcpWriteXmit, "tcp_write_xmit"},
		{types.StageTcpTransmitSkb, "tcp_transmit_skb"},
		{types.StageToDevQ, "to_devq"},
	}
	for _, tc := range cases {
		t.Run(tc.stageName, func(t *testing.T) {
			resetMetrics()
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(stageLatencyLabeled)

			ev := sampleEvent(podID("ns-src", "src-pod", "uid-src"), podID("ns-dst", "dst-pod", "uid-dst"), tc.stage, tc.stageName)
			Record(ev)

			// histogram series 가 정확히 1 개 emit 되어야 한다 (label set 단일).
			got := labelValue(t, reg, "netobs_stage_latency_labeled_seconds", "stage")
			if got != tc.stageName {
				t.Errorf("stage label=%q want %q", got, tc.stageName)
			}
		})
	}
}

// TestRecord_RecvPathStageLatency 는 receive path stage event 가 stageLatencyLabeled histogram 에
// latency 를 emit 하는지 검증한다. #141 의 3 종 (rcv_demux, rcv_established, rcv_app) 에 더해 #173 의
// rcv_nic (NIC ingress→L3) 까지 포함한다. 기존에는 rcv stage 가 TCP 상태 sample 만 집계하고 latency 를
// Observe 하지 않아 dashboard 수신 패널이 빈 데이터로 남던 회귀를 가드한다.
func TestRecord_RecvPathStageLatency(t *testing.T) {
	cases := []struct {
		stage     uint8
		stageName string
	}{
		{types.StageRcvNic, "rcv_nic"},
		{types.StageRcvDemux, "rcv_demux"},
		{types.StageRcvEstablished, "rcv_established"},
		{types.StageRcvApp, "rcv_app"},
	}
	for _, tc := range cases {
		t.Run(tc.stageName, func(t *testing.T) {
			resetMetrics()
			reg := prometheus.NewPedanticRegistry()
			reg.MustRegister(stageLatencyLabeled)

			ev := sampleEvent(podID("ns-src", "src-pod", "uid-src"), podID("ns-dst", "dst-pod", "uid-dst"), tc.stage, tc.stageName)
			Record(ev)

			// latency 를 Observe 하지 않으면 series 자체가 없어 빈 라벨 값이 반환되어 실패한다.
			got := labelValue(t, reg, "netobs_stage_latency_labeled_seconds", "stage")
			if got != tc.stageName {
				t.Errorf("stage label=%q want %q (%s latency 미Observe)", got, tc.stageName, tc.stageName)
			}
		})
	}
}

// podID/serviceID/externalID는 dst_labels_test.go 와 동일 형태의 헬퍼이며, 두 패키지 간 reuse 가
// 불가능 (test-only) 해 본 파일에 재정의한다.
func podID(ns, name, uid string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassPod,
		Namespace:     ns,
		PodName:       name,
		Workload:      name,
		WorkloadKind:  "Deployment",
		PodUID:        uid,
	}
}

func serviceID(ns, name string) kube.PodIdentity {
	return kube.PodIdentity{
		IdentityClass: kube.IdentityClassService,
		Namespace:     ns,
		Workload:      name,
	}
}

func externalID() kube.PodIdentity {
	return kube.PodIdentity{IdentityClass: kube.IdentityClassExternal}
}

// TestDropWallSeconds 는 #142 의 monotonic ts_ns → wall-clock unix seconds 변환을 검증한다. offset
// 미설정 시 monotonic 값을 그대로 초 변환하고, offset 설정 시 boot 기준 ts 가 unix epoch 으로 환산
// 되는지 회귀 가드한다.
func TestDropWallSeconds(t *testing.T) {
	t.Cleanup(func() { SetDropClockOffset(0) })

	// 큰 offset 의 float64 변환은 ns 단위 반올림 오차가 있어 동등 비교 대신 허용 오차 (1ms) 로 가드한다.
	approx := func(got, want float64) bool { return math.Abs(got-want) <= 1e-3 }

	SetDropClockOffset(0)
	if got := dropWallSeconds(2_000_000_000); !approx(got, 2.0) {
		t.Errorf("offset 0: dropWallSeconds(2e9 ns)=%v want ~2.0", got)
	}

	// boot 시각이 unix 1700000000s 였다고 가정한 offset. ts_ns=0 (boot 순간) 은 그 wall 시각으로 환산.
	SetDropClockOffset(1_700_000_000_000_000_000)
	if got := dropWallSeconds(0); !approx(got, 1.7e9) {
		t.Errorf("offset 적용: dropWallSeconds(0)=%v want ~1.7e9", got)
	}
	if got := dropWallSeconds(5_000_000_000); !approx(got, 1_700_000_005.0) {
		t.Errorf("offset+ts: dropWallSeconds(5e9 ns)=%v want ~1700000005.0", got)
	}
}

// TestRecord_DropTimestampGauge 는 #142 의 StageDrop 분기가 dropFlowGuard 통과 시 dropLastTimestamp
// gauge 를 emit 하고, allow-list 밖 namespace 는 emit 하지 않는지 검증한다. counter (dropEventsFlow)
// 와 동일한 가드 정책이 시점 gauge 에도 적용됨을 회귀 가드한다.
func TestRecord_DropTimestampGauge(t *testing.T) {
	dropLastTimestamp.Reset()
	dropEventsFlow.Reset()
	// offset 이 0 이면 #142 의 gauge Set 이 skip 되므로 (CLOCK_MONOTONIC 실패 fallback), 정상 운영처럼
	// 0 이 아닌 offset 을 주입해 gauge emit 경로를 검증한다.
	SetDropClockOffset(1_700_000_000_000_000_000)
	SetDropFlowGuard(NewDropFlowGuard([]string{"ns-src"}, 100))
	t.Cleanup(func() {
		SetDropFlowGuard(nil)
		SetDropClockOffset(0)
		dropLastTimestamp.Reset()
		dropEventsFlow.Reset()
	})

	ev := types.EnrichedEvent{
		Raw:            types.Event{Stage: types.StageDrop, TsNs: 5_000_000_000},
		Stage:          "drop",
		Direction:      "ingress",
		TrafficScope:   "pod_to_pod",
		ObservedNode:   "node-a",
		Src:            podID("ns-src", "src-pod", "uid-src"),
		SrcIPText:      "10.0.0.1",
		DstIPText:      "10.0.0.2",
		ProtocolText:   "TCP",
		DropReasonName: "TCP_INVALID_SEQUENCE",
		DropCategory:   "protocol",
	}
	Record(ev)

	if got := testutil.CollectAndCount(dropLastTimestamp); got != 1 {
		t.Fatalf("allow-list 통과 dropLastTimestamp series=%d want 1", got)
	}

	// allow-list 밖 namespace 의 drop 은 gauge 도 emit 되지 않아야 한다.
	dropLastTimestamp.Reset()
	SetDropFlowGuard(NewDropFlowGuard([]string{"other-ns"}, 100))
	Record(ev)
	if got := testutil.CollectAndCount(dropLastTimestamp); got != 0 {
		t.Errorf("allow-list 밖 dropLastTimestamp series=%d want 0 (가드 적용)", got)
	}
}

// TestRecord_SockTeardownSeparatedFromDrop 은 #345 의 소켓 종료 정리 분리를 검증한다. StageSockTeardown
// 이벤트는 전용 netobs_sock_teardown_total 로만 집계되고 netobs_drop_events_labeled_total 에는
// 어떤 시리즈도 남기지 않아야 한다 (drop 노이즈 분리). StageDrop 은 반대로 drop 메트릭만 증가시킨다.
func TestRecord_SockTeardownSeparatedFromDrop(t *testing.T) {
	resetMetrics()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(sockTeardownLabeled)
	reg.MustRegister(dropEventsLabeled)

	src := podID("kube-system", "coredns", "uid-c")
	dst := externalID()
	Record(sampleEvent(src, dst, types.StageSockTeardown, "sock_teardown"))

	if got := testutil.CollectAndCount(sockTeardownLabeled); got != 1 {
		t.Errorf("netobs_sock_teardown_total series=%d want 1", got)
	}
	if got := testutil.CollectAndCount(dropEventsLabeled); got != 0 {
		t.Errorf("netobs_drop_events_labeled_total series=%d want 0 (teardown 은 drop 이 아님)", got)
	}
	if v := labelValue(t, reg, "netobs_sock_teardown_total", "src_workload"); v != "coredns" {
		t.Errorf("src_workload=%q want coredns", v)
	}

	// 대조: StageDrop 은 drop 메트릭만 증가시키고 teardown 메트릭은 건드리지 않는다.
	resetMetrics()
	reg = prometheus.NewPedanticRegistry()
	reg.MustRegister(sockTeardownLabeled)
	reg.MustRegister(dropEventsLabeled)
	Record(sampleEvent(src, dst, types.StageDrop, "drop"))
	if got := testutil.CollectAndCount(dropEventsLabeled); got != 1 {
		t.Errorf("drop series=%d want 1", got)
	}
	if got := testutil.CollectAndCount(sockTeardownLabeled); got != 0 {
		t.Errorf("teardown series=%d want 0 for a real drop", got)
	}
}
