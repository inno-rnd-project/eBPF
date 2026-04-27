// Package types는 netobs eBPF 이벤트 모델과 enrichment 결과를 정의한다.
// PodIdentity 등 클러스터 식별 모델은 netobs/gpuobs 공용 패키지인 internal/kube에 있으며,
// 본 패키지는 그 위에 eBPF stage/이벤트 표현을 얹는 역할만 한다.
package types

import (
	"bytes"
	"encoding/binary"
	"net"

	"netobs/internal/kube"
)

const (
	StageSendmsgRet = 1
	StageToVeth     = 2
	StageToDevQ     = 3
	StageRetrans    = 4
	StageDrop       = 5
)

type Event struct {
	TsNs         uint64
	CgroupID     uint64
	SocketCookie uint64

	Saddr     uint32
	Daddr     uint32
	Pid       uint32
	Tid       uint32
	Ret       uint32
	LatencyUs uint32
	Reason    uint32

	Ifindex uint32
	SkbIif  uint32

	Sport uint16
	Dport uint16
	Comm  [16]byte
	Stage uint8
	Pad   [3]byte
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
	Src            kube.PodIdentity
	Dst            kube.PodIdentity
	DropReasonName string
	DropCategory   string
}

// SourceNamespaceLabel/SourceWorkloadLabel은 Src PodIdentity 메서드를 메트릭 호출부에서
// 짧게 쓰기 위한 위임자다.
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

// U32ToIPv4는 BPF가 network byte order 바이트를 네이티브 엔디언 uint32로 기록한 값을
// 사람이 읽을 수 있는 IPv4 문자열로 변환한다. NativeEndian으로 재구성하면 LE/BE 양쪽에서
// 항상 올바른 결과가 나온다.
func U32ToIPv4(v uint32) string {
	var b [4]byte
	binary.NativeEndian.PutUint32(b[:], v)
	return net.IPv4(b[0], b[1], b[2], b[3]).String()
}
