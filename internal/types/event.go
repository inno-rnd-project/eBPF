package types

import (
	"bytes"
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
