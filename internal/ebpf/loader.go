package ebpfx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log"
	"net"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"

	"netobs/internal/types"
)

func ipToU32(ipStr string) (uint32, error) {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return 0, errors.New("invalid IPv4")
	}
	return binary.BigEndian.Uint32(ip), nil
}

func attachRequiredKprobe(symbol string, prog *cebpf.Program, links *[]link.Link) error {
	l, err := link.Kprobe(symbol, prog, nil)
	if err != nil {
		return err
	}
	*links = append(*links, l)
	log.Printf("attached kprobe/%s", symbol)
	return nil
}

func attachRequiredKretprobe(symbol string, prog *cebpf.Program, links *[]link.Link) error {
	l, err := link.Kretprobe(symbol, prog, nil)
	if err != nil {
		return err
	}
	*links = append(*links, l)
	log.Printf("attached kretprobe/%s", symbol)
	return nil
}

func attachOptionalKprobe(symbol string, prog *cebpf.Program, links *[]link.Link) {
	l, err := link.Kprobe(symbol, prog, nil)
	if err != nil {
		log.Printf("skip optional kprobe/%s: %v", symbol, err)
		return
	}
	*links = append(*links, l)
	log.Printf("attached kprobe/%s", symbol)
}

func Run(ctx context.Context, targetIP string, out chan<- types.Event) error {
	defer close(out)

	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	var objs NetObsObjects
	if err := LoadNetObsObjects(&objs, nil); err != nil {
		return err
	}
	defer objs.Close()

	if targetIP != "" {
		val, err := ipToU32(targetIP)
		if err != nil {
			return err
		}
		var key uint32 = 0
		if err := objs.TargetDaddr.Update(key, val, cebpf.UpdateAny); err != nil {
			return err
		}
		log.Printf("target daddr filter enabled: %s", targetIP)
	} else {
		log.Printf("target daddr filter disabled")
	}

	var links []link.Link
	defer func() {
		for _, l := range links {
			_ = l.Close()
		}
	}()

	if err := attachRequiredKprobe("tcp_sendmsg", objs.HandleTcpSendmsg, &links); err != nil {
		return err
	}
	if err := attachRequiredKretprobe("tcp_sendmsg", objs.HandleTcpSendmsgRet, &links); err != nil {
		return err
	}

	attachOptionalKprobe("veth_xmit", objs.HandleVethXmit, &links)
	attachOptionalKprobe("__dev_queue_xmit", objs.HandleDevQueueXmit, &links)
	attachOptionalKprobe("tcp_retransmit_skb", objs.HandleTcpRetransmitSkb, &links)
	attachOptionalKprobe("kfree_skb_reason", objs.HandleKfreeSkbReason, &links)

	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		return err
	}
	defer rd.Close()

	go func() {
		<-ctx.Done()
		_ = rd.Close()
	}()

	for {
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		var ev types.Event
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.LittleEndian, &ev); err != nil {
			log.Printf("decode ringbuf event: %v", err)
			continue
		}

		select {
		case out <- ev:
		case <-ctx.Done():
			return nil
		}
	}
}
