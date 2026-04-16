package types

import (
	"bytes"
	"fmt"
	"net"
	"strings"
)

const (
	StageSendmsgRet = 1
	StageToVeth     = 2
	StageToDevQ     = 3
	StageRetrans    = 4
	StageDrop       = 5
)

const (
	IdentityClassPod        = "pod"
	IdentityClassNode       = "node"
	IdentityClassService    = "service"
	IdentityClassExternal   = "external"
	IdentityClassUnresolved = "unresolved"
)

type Event struct {
	TsNs      uint64
	CgroupID  uint64
	Saddr     uint32
	Daddr     uint32
	Pid       uint32
	Tid       uint32
	Ret       uint32
	LatencyUs uint32
	Reason    uint32
	Sport     uint16
	Dport     uint16
	Comm      [16]byte
	Stage     uint8
	Pad       [3]byte
}

type PodIdentity struct {
	IdentityClass string
	Namespace     string
	PodName       string
	NodeName      string
	WorkloadKind  string
	Workload      string
	PodIP         string
}

func (p PodIdentity) Known() bool {
	return p.IdentityClass == IdentityClassPod
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

func (p PodIdentity) WorkloadLabel() string {
	switch p.IdentityClass {
	case IdentityClassPod:
		// Deployment/StatefulSet/DaemonSet 등 owner가 있으면 그 이름 사용
		if p.Workload != "" {
			name := normalizeWorkloadName(p.WorkloadKind, p.Workload)
			if name != "" {
				return name
			}
		}
		// owner가 없으면 pod 이름이라도 남겨서 unknown 남발 방지
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

func normalizeWorkloadName(kind, name string) string {
	if name == "" {
		return ""
	}

	// StatefulSet 이름은 원래 안정적이므로 그대로 둔다.
	if kind == "StatefulSet" {
		return name
	}

	if trimmed := trimGeneratedSuffix(name); trimmed != "" {
		return trimmed
	}
	return name
}

func trimGeneratedSuffix(name string) string {
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

func (p PodIdentity) NodeLabel() string {
	if p.NodeName == "" {
		return "unknown"
	}
	return p.NodeName
}

// namespace/pod 또는 node/노드명 또는 svc/서비스명 또는 IP 등 최대한 식별 가능한 형태로 표현
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

type EnrichedEvent struct {
	Raw            Event
	Stage          string
	CommText       string
	Direction      string
	TrafficScope   string
	ObservedNode   string
	SrcIPText      string
	DstIPText      string
	Src            PodIdentity
	Dst            PodIdentity
	DropReasonName string
	DropCategory   string
}

func (e EnrichedEvent) SourceNamespaceLabel() string {
	return e.Src.NamespaceLabel()
}

func (e EnrichedEvent) SourceWorkloadLabel() string {
	return e.Src.WorkloadLabel()
}

func (e EnrichedEvent) ObservedNodeLabel() string {
	if e.ObservedNode == "" {
		return "unknown"
	}
	return e.ObservedNode
}

func StageName(stage uint8) string {
	switch stage {
	case StageSendmsgRet:
		return "sendmsg_ret"
	case StageToVeth:
		return "to_veth"
	case StageToDevQ:
		return "to_devq"
	case StageRetrans:
		return "retrans"
	case StageDrop:
		return "drop"
	default:
		return "unknown"
	}
}

func CommString(comm [16]byte) string {
	n := bytes.IndexByte(comm[:], 0)
	if n == -1 {
		n = len(comm)
	}
	return string(comm[:n])
}

func U32ToIPv4(v uint32) string {
	ip := net.IPv4(
		byte(v>>24),
		byte(v>>16),
		byte(v>>8),
		byte(v),
	)
	return ip.String()
}
