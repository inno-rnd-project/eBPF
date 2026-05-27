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
	StageSendmsgRet     = 1
	StageToVeth         = 2
	StageToDevQ         = 3
	StageRetrans        = 4
	StageDrop           = 5
	StageRcvL3          = 6
	StageRcvDemux       = 7
	StageRcvEstablished = 8
	StageRcvApp         = 9
	// #82 send path stage 4 분해. tcp_sendmsg (= StageSendmsgRet) 와 __dev_queue_xmit
	// (= StageToDevQ) 사이의 두 stage 다. tcp_write_xmit 는 TCP control path, tcp_transmit_skb
	// 는 개별 segment transmit entry 라 TSO/GSO 활성 시 첫 segment 만 latency 가 측정된다.
	StageTcpWriteXmit   = 10
	StageTcpTransmitSkb = 11
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
	// Protocol 은 IP protocol number (IPPROTO_TCP=6, IPPROTO_UDP=17) 다. #64 에서 BPF 측 netobs_event
	// 의 기존 pad[0] 슬롯에 들어가 본 필드 추가만으로는 struct size 가 변하지 않았다 (#65 의 TCP 상태
	// 필드로 struct 전체는 별도로 확장됨).
	Protocol uint8
	Pad      [2]byte

	// #65 TCP 상태 메트릭. rcv_* stage 의 emit 에서만 채워지며 그 외 stage 는 0. SrttUs 는 kernel
	// 의 << 3 scale 을 BPF 단에서 >> 3 한 실제 µs 단위라 추가 변환이 필요 없다. 본 3 필드 추가로
	// struct size 가 88 → 96 byte 로 확장되며 BPF 측 netobs_event 와 정합한다. #83 에서 StackID 와
	// Pad83 추가로 96 → 104 byte 로 재확장된다.
	SndCwnd     uint32
	SrttUs      uint32
	SndSsthresh uint32

	// #83 drop event 의 kernel stack capture. handle_kfree_skb_reason 의 bpf_get_stackid 반환값을
	// 그대로 carry 하며 drop 외 stage 는 -1 로 명시 가드된다. BPF 측 netobs_event 의 stack_id 와 정합
	// 하고, Pad83 슬롯은 컴파일러의 8-byte align trailing padding 을 명시 선언해 C / Go layout
	// 일관성을 확보한다.
	StackID int32
	Pad83   [4]byte
}

type EnrichedEvent struct {
	Raw          Event
	Stage        string
	CommText     string
	Direction    string
	TrafficScope string
	ObservedNode string
	SrcIPText    string
	DstIPText    string
	// ProtocolText 는 IP protocol number 의 사람 읽을 수 있는 라벨이다. drop flow 5-tuple 메트릭의
	// protocol 라벨로 사용된다. 알려지지 않은 protocol 은 "unknown" 으로 표시한다.
	ProtocolText   string
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
	case StageRcvL3:
		return "rcv_l3"
	case StageRcvDemux:
		return "rcv_demux"
	case StageRcvEstablished:
		return "rcv_established"
	case StageRcvApp:
		return "rcv_app"
	case StageTcpWriteXmit:
		return "tcp_write_xmit"
	case StageTcpTransmitSkb:
		return "tcp_transmit_skb"
	default:
		return "unknown"
	}
}

// StageDirection 은 stage 별 흐름 방향을 반환한다. send path 7 종 (#82 의 tcp_write_xmit / tcp_
// transmit_skb 포함) 은 "egress", #65 의 rcv path 4 종은 "ingress" 로 분류한다. enricher 가
// Direction 라벨 산정에 사용하며, 알 수 없는 stage 는 "unknown" 으로 둬 메트릭 라벨이 빈 문자열
// 로 비지 않게 한다.
func StageDirection(stage uint8) string {
	switch stage {
	case StageSendmsgRet, StageToVeth, StageToDevQ, StageRetrans, StageDrop,
		StageTcpWriteXmit, StageTcpTransmitSkb:
		return "egress"
	case StageRcvL3, StageRcvDemux, StageRcvEstablished, StageRcvApp:
		return "ingress"
	}
	return "unknown"
}

// IPProtocolName 은 IP protocol number 의 사람 읽을 수 있는 라벨을 반환한다. 첫 구현은 본 시리즈가
// 다루는 TCP / UDP 두 protocol 만 매핑하고 그 외는 "unknown" 으로 두어 drop flow 메트릭의 카디널
// 리티가 알려진 protocol 셋 안에서만 늘어나도록 한다.
func IPProtocolName(p uint8) string {
	switch p {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	}
	return "unknown"
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
