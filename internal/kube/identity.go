// Package kube는 netobs/gpuobs가 공통으로 사용하는 Kubernetes 식별/해석 유틸을 제공한다.
// PodIdentity 등 식별 모델과 informer 기반 IP→identity Resolver를 한 곳으로 모아,
// netobs(Event 기반 enrich)와 gpuobs(PID 기반 Pod 귀속)가 동일한 식별 어휘를 공유하도록 한다.
package kube

import (
	"fmt"
	"strings"
)

// IdentityClass는 PodIdentity가 가리키는 대상의 종류를 분류한다.
// Pod/Service/Node가 식별된 경우와 외부/미해결을 구분해 메트릭 라벨과 traffic scope 판정에 쓰인다.
const (
	IdentityClassPod        = "pod"
	IdentityClassNode       = "node"
	IdentityClassService    = "service"
	IdentityClassExternal   = "external"
	IdentityClassUnresolved = "unresolved"
)

// PodIdentity는 Pod/Service/Node/External/Unresolved 어느 한 쪽으로 분류된 통신 상대를 표현한다.
// 필드는 IdentityClass에 따라 부분적으로 채워지며, NamespaceLabel/WorkloadLabel/WorkloadKey 등은
// 비어 있는 필드를 안전한 기본 라벨("unknown" 등)로 대체해 Prometheus 라벨 카디널리티를 통제한다.
type PodIdentity struct {
	IdentityClass string
	Namespace     string
	PodUID        string
	PodName       string
	NodeName      string
	WorkloadKind  string
	Workload      string
	PodIP         string
	// HostNetwork 는 hostNetwork pod 여부다 (#321). 이 pod 의 트래픽은 전용 veth 가 아닌 노드
	// 공유 host 인터페이스를 지나므로 enricher 가 ifindex 힌트 학습 대상에서 제외한다.
	HostNetwork bool
}

// Known은 PodIdentity가 미해결 상태가 아닌지 반환한다.
func (p PodIdentity) Known() bool {
	return !p.IsUnresolved()
}

func (p PodIdentity) IsPod() bool {
	return p.IdentityClass == IdentityClassPod
}

func (p PodIdentity) IsNode() bool {
	return p.IdentityClass == IdentityClassNode
}

func (p PodIdentity) IsService() bool {
	return p.IdentityClass == IdentityClassService
}

func (p PodIdentity) IsExternal() bool {
	return p.IdentityClass == IdentityClassExternal
}

func (p PodIdentity) IsUnresolved() bool {
	return p.IdentityClass == IdentityClassUnresolved || p.IdentityClass == ""
}

// NamespaceLabel은 Prometheus 라벨에 쓸 namespace 표현을 반환한다.
// Pod/Service에는 실제 namespace, Node에는 "host", External/Unresolved에는 분류명 자체를 사용한다.
func (p PodIdentity) NamespaceLabel() string {
	switch p.IdentityClass {
	case IdentityClassPod:
		if p.Namespace == "" {
			return "unknown"
		}
		return p.Namespace
	case IdentityClassNode:
		return "host"
	case IdentityClassService:
		if p.Namespace == "" {
			return "service"
		}
		return p.Namespace
	case IdentityClassExternal:
		return "external"
	case IdentityClassUnresolved:
		return "unresolved"
	default:
		return "unknown"
	}
}

// WorkloadLabel은 Prometheus 라벨에 쓸 워크로드 표현을 반환한다.
// Deployment/StatefulSet/DaemonSet 등 owner가 있으면 정규화된 owner 이름을, 없으면 Pod 이름을 사용해
// "unknown" 남발을 줄인다.
func (p PodIdentity) WorkloadLabel() string {
	switch p.IdentityClass {
	case IdentityClassPod:
		if p.Workload != "" {
			name := normalizeWorkloadName(p.WorkloadKind, p.Workload)
			if name != "" {
				return name
			}
		}
		if p.PodName != "" {
			return p.PodName
		}
		return "unknown"

	case IdentityClassNode:
		if p.NodeName != "" {
			return "node/" + p.NodeName
		}
		return "host-network"

	case IdentityClassService:
		if p.Workload != "" {
			return "svc/" + p.Workload
		}
		return "service"

	case IdentityClassExternal:
		return "external"

	case IdentityClassUnresolved:
		return "unresolved"

	default:
		return "unknown"
	}
}

// normalizeWorkloadName은 ReplicaSet의 generated hash suffix를 제거해 부모 Deployment 이름을 복원한다.
// hashlike 휴리스틱은 길이만 보고 영숫자 suffix를 자르므로 ReplicaSet 외 워크로드(예: DaemonSet "node-exporter",
// 안정 이름 Job 등)에 적용하면 false-positive로 의미 있는 이름까지 잘려나간다. 이를 방지하기 위해
// ReplicaSet에만 trim을 시도하고 그 외에는 입력 이름을 그대로 보존한다.
func normalizeWorkloadName(kind, name string) string {
	if name == "" {
		return ""
	}

	if kind != "ReplicaSet" {
		return name
	}

	if trimmed := TrimGeneratedSuffix(name); trimmed != "" {
		return trimmed
	}
	return name
}

// TrimGeneratedSuffix는 ReplicaSet/Deployment 자동 생성 hash suffix(예: -7d4f9b8c5)를 제거한 부모 이름을 반환한다.
// suffix 형태가 hash-like가 아니면 빈 문자열을 돌려준다.
func TrimGeneratedSuffix(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return ""
	}

	last := parts[len(parts)-1]
	if !isHashLikeSuffix(last) {
		return ""
	}

	return strings.Join(parts[:len(parts)-1], "-")
}

func isHashLikeSuffix(s string) bool {
	if len(s) < 8 || len(s) > 16 {
		return false
	}

	for _, ch := range s {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			continue
		}
		return false
	}
	return true
}

// NodeLabel은 Prometheus 라벨에 쓸 노드 표현을 반환한다.
func (p PodIdentity) NodeLabel() string {
	if p.NodeName == "" {
		return "unknown"
	}
	return p.NodeName
}

// String은 사람이 읽을 수 있는 식별 문자열을 반환한다.
// namespace/pod, node/노드명, svc/서비스명 등 IdentityClass에 따라 형식을 달리하며,
// 정보가 부족하면 PodIP fallback 후 분류명으로 폴백한다.
func (p PodIdentity) String() string {
	switch p.IdentityClass {
	case IdentityClassPod:
		if p.Namespace != "" && p.PodName != "" {
			return fmt.Sprintf("%s/%s", p.Namespace, p.PodName)
		}
		if p.PodIP != "" {
			return p.PodIP
		}
		return "pod"
	case IdentityClassNode:
		if p.NodeName != "" {
			return "node/" + p.NodeName
		}
		if p.PodIP != "" {
			return p.PodIP
		}
		return "host"
	case IdentityClassService:
		if p.Namespace != "" && p.Workload != "" {
			return fmt.Sprintf("%s/svc/%s", p.Namespace, p.Workload)
		}
		if p.Workload != "" {
			return "svc/" + p.Workload
		}
		if p.PodIP != "" {
			return p.PodIP
		}
		return "service"
	case IdentityClassExternal:
		if p.PodIP != "" {
			return p.PodIP
		}
		return "external"
	case IdentityClassUnresolved:
		if p.PodIP != "" {
			return p.PodIP
		}
		return "unresolved"
	default:
		if p.PodIP != "" {
			return p.PodIP
		}
		return "unknown"
	}
}

// Rank는 두 식별이 충돌할 때 어느 쪽을 더 신뢰할지 결정하는 우선순위를 반환한다.
// Pod > Service > Node > External > Unresolved 순으로 강한 식별이다.
func (p PodIdentity) Rank() int {
	switch p.IdentityClass {
	case IdentityClassPod:
		return 5
	case IdentityClassService:
		return 4
	case IdentityClassNode:
		return 3
	case IdentityClassExternal:
		return 2
	case IdentityClassUnresolved:
		return 1
	default:
		return 0
	}
}

// WorkloadKey는 namespace/kind/workload 3단으로 안정적인 워크로드 키를 만들어
// 메트릭 라벨 그룹화와 식별 dedup의 키로 쓸 수 있게 한다.
func (p PodIdentity) WorkloadKey() string {
	switch p.IdentityClass {
	case IdentityClassPod:
		kind := p.WorkloadKind
		if kind == "" {
			kind = "Pod"
		}
		return p.NamespaceLabel() + "/" + kind + "/" + p.WorkloadLabel()
	case IdentityClassService:
		return p.NamespaceLabel() + "/Service/" + p.WorkloadLabel()
	case IdentityClassNode:
		return "host/Node/" + p.WorkloadLabel()
	case IdentityClassExternal:
		return "external/External/external"
	case IdentityClassUnresolved:
		return "unresolved/Unresolved/unresolved"
	default:
		return "unknown/Unknown/unknown"
	}
}
