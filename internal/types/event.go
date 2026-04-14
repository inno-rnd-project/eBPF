package types

import (
	"bytes"
	"fmt"
	"net"
)

const (
	StageSendmsgRet = 1
	StageToVeth     = 2
	StageToDevQ     = 3
	StageRetrans    = 4
	StageDrop       = 5
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
	Namespace    string
	PodName      string
	NodeName     string
	WorkloadKind string
	Workload     string
	PodIP        string
}

func (p PodIdentity) Known() bool {
	return p.Namespace != "" || p.PodName != "" || p.PodIP != ""
}

func (p PodIdentity) NamespaceLabel() string {
	if p.Namespace == "" {
		return "unknown"
	}
	return p.Namespace
}

func (p PodIdentity) WorkloadLabel() string {
	switch {
	case p.Workload != "":
		return p.Workload
	case p.PodName != "":
		return p.PodName
	default:
		return "unknown"
	}
}

func (p PodIdentity) NodeLabel() string {
	if p.NodeName == "" {
		return "unknown"
	}
	return p.NodeName
}

func (p PodIdentity) String() string {
	switch {
	case p.Namespace != "" && p.PodName != "":
		return fmt.Sprintf("%s/%s", p.Namespace, p.PodName)
	case p.PodIP != "":
		return p.PodIP
	default:
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
