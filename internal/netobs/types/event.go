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
	// #173 NIC ingress→L3 stage. __netif_receive_skb (NIC 드라이버의 커널 stack 진입점) 부터
	// tcp_v4_rcv / tcp_v6_rcv (L3 진입) 까지의 softirq 처리 시간이다. rcv path 4 종 (6-9) 의 가장
	// 앞 단 segment 로, BPF 측 netobs_event_stage 의 NETOBS_STAGE_RCV_NIC 와 정합한다.
	StageRcvNic = 12
	// #197 수신측 ACK 대기. tcp_rcv_established 가 첫 미-ACK 데이터 수신 시각을 stash 하고 tcp_send_ack
	// (지연 ACK / quickack 로 standalone ACK 를 송신하는 지점) 가 그 차분을 대기 latency 로 emit 한다.
	// rcv path 계열이라 ingress 로 분류되며 stage 라벨은 "ack_wait" 다.
	StageAckWait = 13
	// #227 client 측 TCP 연결 수립 지연. tcp_v4_connect / tcp_v6_connect 진입부터 tcp_finish_connect
	// (SYN-ACK 수신 처리로 established 전환) 까지로, 네트워크 왕복과 커널 처리를 합친 서비스 지연의 첫
	// 구간이다. client 발신 개시라 egress 로 분류한다.
	StageConnect = 14
)

type Event struct {
	TsNs         uint64
	CgroupID     uint64
	SocketCookie uint64

	// Saddr / Daddr 은 #103 IPv6 확장 으로 [16]byte 통합 슬롯 으로 변경 되었다. IPv4 는 첫 4 byte 만
	// 사용 하며 나머지 12 byte 는 0 으로 초기화 된다. IPToString helper 가 Family 필드 기준 으로
	// IPv4 / IPv6 표현 으로 변환 한다.
	Saddr     [16]byte
	Daddr     [16]byte
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
	// Protocol 은 IP protocol number (IPPROTO_TCP=6, IPPROTO_UDP=17) 다.
	Protocol uint8
	// Family 는 #103 IPv6 확장 으로 추가 된 NETOBS_AF_INET (2) 또는 NETOBS_AF_INET6 (10) 식별자 다.
	// userspace 가 ip_version 라벨 산정 (2 → "4", 10 → "6") 에 사용 한다.
	Family uint8
	Pad    uint8

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

	// #121 TSO/GSO send path segment 누적 latency 와 segment 개수. sendmsg_ret stage 에서만 0 이 아닌
	// 값을 가지며 다른 stage 는 0 으로 emit 된다. FullLatencyNs 는 sendmsg 사이클 의 모든
	// tcp_transmit_skb segment latency 합산 (nanoseconds), SegmentCount 는 tcp_transmit_skb 호출
	// 횟수 다. TSO/GSO 활성 환경 의 large message 가 multi-segment 로 분할 될 때 SegmentCount > 1
	// 이 된다. 본 2 필드 추가로 struct size 가 104 → 120 byte 로 확장 되며 BPF 측 netobs_event 와
	// 정합 한다.
	FullLatencyNs uint64
	SegmentCount  uint32
	Pad121        [4]byte
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
	DropStage      string
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
	case StageRcvNic:
		return "rcv_nic"
	case StageRcvL3:
		return "rcv_l3"
	case StageRcvDemux:
		return "rcv_demux"
	case StageRcvEstablished:
		return "rcv_established"
	case StageRcvApp:
		return "rcv_app"
	case StageAckWait:
		return "ack_wait"
	case StageConnect:
		return "connect"
	case StageTcpWriteXmit:
		return "tcp_write_xmit"
	case StageTcpTransmitSkb:
		return "tcp_transmit_skb"
	default:
		return "unknown"
	}
}

// StageDirection 은 stage 별 흐름 방향을 반환한다. send path 7 종 (#82 의 tcp_write_xmit / tcp_
// transmit_skb 포함) 은 "egress", rcv path 5 종 (#65 의 4 종 + #173 의 rcv_nic) 은 "ingress" 로
// 분류한다. enricher 가
// Direction 라벨 산정에 사용하며, 알 수 없는 stage 는 "unknown" 으로 둬 메트릭 라벨이 빈 문자열
// 로 비지 않게 한다.
func StageDirection(stage uint8) string {
	switch stage {
	case StageSendmsgRet, StageToVeth, StageToDevQ, StageRetrans, StageDrop,
		StageTcpWriteXmit, StageTcpTransmitSkb, StageConnect:
		return "egress"
	case StageRcvNic, StageRcvL3, StageRcvDemux, StageRcvEstablished, StageRcvApp, StageAckWait:
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

// IPVersion 은 #103 IPv6 확장 의 ip_version 라벨 값 (4 또는 6) 을 반환 한다. family 가 IPv4 /
// IPv6 외 값 이면 빈 문자열 반환. ip_version 라벨 산정 의 단일 진입점.
func IPVersion(family uint8) string {
	switch family {
	case 2: // NETOBS_AF_INET
		return "4"
	case 10: // NETOBS_AF_INET6
		return "6"
	}
	return ""
}

// IPToString 은 #103 IPv6 확장 의 통합 슬롯 ([16]byte) 을 family 기준 으로 IPv4 또는 IPv6 표현
// 문자열 로 변환 한다. IPv4 는 첫 4 byte 만 사용 하며 IPv6 는 16 byte 전체 를 net.IP 로 변환.
// 알 수 없는 family 는 빈 문자열 반환. BPF 측 fill_conn_from_sock 가 __builtin_memcpy(saddr,
// &v4_src, 4) 로 IPv4 주소 를 첫 4 byte 에 그대로 복사 하므로 raw[:4] 는 이미 net.IP 가 요구 하는
// 4 byte 네트워크 표현 과 일치 한다 (별도 byte 순서 변환 불요).
func IPToString(family uint8, raw [16]byte) string {
	switch family {
	case 2: // NETOBS_AF_INET
		return net.IP(raw[:4]).String()
	case 10: // NETOBS_AF_INET6
		return net.IP(raw[:]).String()
	}
	return ""
}
