package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

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
		[]string{"node", "src_namespace", "src_workload", "traffic_scope", "direction", "drop_reason", "drop_category", "dst_namespace", "dst_workload"},
	)
	retransEventsLabeled = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "netobs_retrans_events_labeled_total"},
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
	dstClassifier = nil
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
