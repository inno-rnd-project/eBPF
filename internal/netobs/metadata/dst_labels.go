package metadata

import (
	"netobs/internal/kube"
)

// DstLabelClassifier는 트래픽 흐름의 dst peer 정체성을 Prometheus 라벨 셋으로 변환하는 정책
// 단일 진입점이다. enricher가 풀어준 dst PodIdentity, master switch (PodFlowDstEnabled), namespace
// 화이트리스트 (PodFlowDstUIDAllowNamespaces) 세 입력을 받아 dst_namespace / dst_workload /
// dst_pod_uid 세 라벨 값을 결정한다. 모든 dst 라벨 emit 경로 (stage/drop/retrans 메트릭) 는 본
// classifier 만을 거치므로 클러스터 외부 IP 마킹, service IP 표기, 미해상 fallback, UID 게이트가
// 한 곳에서 통일되어 회귀 보호가 쉽다.
type DstLabelClassifier struct {
	enabled bool
	// allow는 dst_pod_uid 가 emit 되는 namespace 집합이다. namespace 미등록 시 dst_pod_uid 는 빈
	// 문자열로 emit 되어 cardinality 가 도입 전 수준으로 유지된다. enabled=false 면 본 집합은 무시
	// 되고 세 라벨 전부가 빈 값이 된다.
	allow map[string]struct{}
}

// NewDstLabelClassifier는 config.Config.PodFlowDstEnabled / PodFlowDstUIDAllowNamespaces 두 값을
// 받아 classifier 인스턴스를 만든다. allowList 의 빈 슬라이스는 nil map 으로 매핑되어 UID 게이트
// 가 일관되게 "어떤 namespace도 허용 안 함" 으로 동작한다.
func NewDstLabelClassifier(enabled bool, allowList []string) *DstLabelClassifier {
	c := &DstLabelClassifier{enabled: enabled}
	if len(allowList) == 0 {
		return c
	}
	c.allow = make(map[string]struct{}, len(allowList))
	for _, ns := range allowList {
		c.allow[ns] = struct{}{}
	}
	return c
}

// 본 모듈이 dst 분류용으로 emit 하는 합성 라벨 값. 실제 namespace 이름과 충돌하지 않도록 underscore
// prefix 규약을 둔다 (Kubernetes는 namespace 이름 첫 글자로 underscore 를 허용하지 않으므로 안전).
const (
	dstExternal   = "_external"
	dstUnresolved = "_unresolved"
)

// Outcome 라벨 값. metrics 패키지의 netobs_dst_classifier_emits_total{outcome} self-observe counter
// 와 동일 문자열을 공유해야 패키지 간 표기가 어긋나지 않는다. metrics 패키지가 본 상수를 mirror
// 한다 (양방향 import 회피).
const (
	OutcomeDisabled      = "disabled"
	OutcomeExternal      = "external"
	OutcomeUnresolved    = "unresolved"
	OutcomeService       = "service"
	OutcomePodWithUID    = "pod_with_uid"
	OutcomePodWithoutUID = "pod_without_uid"
	OutcomeOther         = "other"
)

// Labels는 dst PodIdentity 를 dst_namespace / dst_workload / dst_pod_uid 세 라벨 값과 outcome
// bucket 으로 변환한다. outcome 은 self-observe counter (netobs_dst_classifier_emits_total) 에 기록
// 되어 운영자가 분류 분포를 통해 cardinality bomb 징후를 추적할 수 있게 한다.
//
// 결정 규칙:
// - master switch 꺼짐 → 세 값 모두 빈 문자열, outcome=disabled (cardinality 가 도입 전 수준 유지)
// - dst.IsExternal() → "_external" / "_external" / "", outcome=external
// - dst.IsUnresolved() → "_unresolved" / "_unresolved" / "", outcome=unresolved
// - dst.IsService() → namespace / "svc/<name>" / "", outcome=service
// - dst.IsPod() & allow-list match → namespace / workload / PodUID, outcome=pod_with_uid
// - dst.IsPod() & allow-list miss → namespace / workload / "", outcome=pod_without_uid
// - 그 외 (Node 등) → namespace / workload / "", outcome=other
//
// 본 매핑 단일화 덕에 메트릭 emit 경로에서 분기 로직 없이 세 라벨과 outcome 을 그대로 사용 가능하며,
// cardinality 폭증 가드는 본 함수 내 allow-list 게이트 한 곳으로 수렴한다.
func (c *DstLabelClassifier) Labels(dst kube.PodIdentity) (ns, workload, podUID, outcome string) {
	if c == nil || !c.enabled {
		return "", "", "", OutcomeDisabled
	}

	switch {
	case dst.IsExternal():
		return dstExternal, dstExternal, "", OutcomeExternal

	case dst.IsUnresolved():
		return dstUnresolved, dstUnresolved, "", OutcomeUnresolved

	case dst.IsService():
		return dst.NamespaceLabel(), dst.WorkloadLabel(), "", OutcomeService

	case dst.IsPod():
		ns := dst.NamespaceLabel()
		wl := dst.WorkloadLabel()
		if _, ok := c.allow[dst.Namespace]; ok {
			return ns, wl, dst.PodUID, OutcomePodWithUID
		}
		return ns, wl, "", OutcomePodWithoutUID

	default:
		return dst.NamespaceLabel(), dst.WorkloadLabel(), "", OutcomeOther
	}
}
