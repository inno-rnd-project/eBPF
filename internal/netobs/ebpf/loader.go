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

	"netobs/internal/netobs/metrics"
	"netobs/internal/netobs/types"
)

// trackedSymbols 는 netlat BPF 가 attach 시도할 kprobe / kretprobe 심볼 11 종이다. required 2 종 +
// optional 9 종으로 구성되며 슬라이스 순서는 Run 의 attach 루프 순서와 정합한다. gpuobs 의
// trackedSymbols (cuda loader.go:29) 와 동일하게 metrics.SetBpfProgramLoaded 의 라벨 cardinality
// 를 폐쇄적으로 잡아 attach 단계 이전에도 모든 심볼이 0 으로 선등록되도록 한다.
var trackedSymbols = []string{
	"tcp_sendmsg",
	"tcp_sendmsg_ret",
	"veth_xmit",
	"__dev_queue_xmit",
	"tcp_retransmit_skb",
	"kfree_skb_reason",
	"tcp_cleanup_rbuf",
	"tcp_v4_rcv",
	"tcp_v4_do_rcv",
	"tcp_rcv_established",
	"tcp_recvmsg",
}

func ipToU32(ipStr string) (uint32, error) {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return 0, errors.New("invalid IPv4")
	}
	// BPF의 skc_daddr는 network byte order 바이트를
	// native-endian u32로 읽는다. 호스트 엔디언에 맞춰 변환해야
	// BPF 맵의 target 값과 비교가 성립한다.
	return binary.NativeEndian.Uint32(ip), nil
}

func attachRequiredKprobe(symbol string, prog *cebpf.Program, links *[]link.Link) error {
	l, err := link.Kprobe(symbol, prog, nil)
	if err != nil {
		return err
	}
	*links = append(*links, l)
	metrics.SetBpfProgramLoaded(symbol, true)
	log.Printf("attached kprobe/%s", symbol)
	return nil
}

// attachRequiredKretprobe 는 kretprobe 심볼을 attach 한다. metrics 측 라벨은 동일 심볼명을 쓰지
// 않고 "<symbol>_ret" 접미를 붙여 kretprobe 의 attach 실패가 kprobe 와 구분되어 진단되도록 한다.
// trackedSymbols 의 "tcp_sendmsg_ret" 항목과 정합한다.
func attachRequiredKretprobe(symbol string, prog *cebpf.Program, links *[]link.Link) error {
	l, err := link.Kretprobe(symbol, prog, nil)
	if err != nil {
		return err
	}
	*links = append(*links, l)
	metrics.SetBpfProgramLoaded(symbol+"_ret", true)
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
	metrics.SetBpfProgramLoaded(symbol, true)
	log.Printf("attached kprobe/%s", symbol)
}

// Runtime은 Run이 onReady 콜백으로 상위에 넘기는 BPF 런타임 핸들이다. ringbuf events는 채널로 따로
// 흘러가므로 Runtime에는 외부 컴포넌트가 scrape 등 다른 lifecycle에서 직접 읽어야 하는 BPF 맵만 노출한다.
type Runtime struct {
	// PodBytes는 (cgroup_id, direction, layer) 키로 누적되는 LRU PERCPU HASH 맵으로,
	// podbytes collector가 scrape 시점에 iterate해 Prometheus counter로 emit한다.
	PodBytes *cebpf.Map
}

// Run은 BPF 오브젝트 로드, 프로브 attach, ringbuf reader 준비가 모두 끝난 시점에
// onReady를 호출해 상위에 readiness를 알리고 Runtime 핸들을 넘긴다. onReady가 nil이면 무시한다.
func Run(ctx context.Context, targetIP string, out chan<- types.Event, onReady func(*Runtime)) error {
	defer close(out)

	// 진단 시그널 일관성: attach 시도 자체가 일어나기 전에 모든 심볼을 0 으로 선등록해 둔다.
	// LoadNetObsObjects 실패 / capability 부족 등으로 attach 단계조차 못 가는 상황도 운영자가
	// netobs_bpf_program_loaded 메트릭만 보고 진단할 수 있다 (gpuobs cuda loader 의 동일 패턴).
	for _, sym := range trackedSymbols {
		metrics.SetBpfProgramLoaded(sym, false)
	}

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
	attachOptionalKprobe("tcp_cleanup_rbuf", objs.HandleTcpCleanupRbuf, &links)

	// #65 receive path 의 BPF kprobe 4 종. tcp_v4_do_rcv / tcp_rcv_established / tcp_recvmsg 3 종은
	// sock 인자가 있어 emit_rcv_event 로 stage 별 진입 시점과 TCP 상태를 ringbuf 에 emit 하고,
	// tcp_v4_rcv 는 socket lookup 이전이라 sock 이 없어 attach 만 유지하고 event emit 은 보류한다.
	// attachOptionalKprobe 를 사용해 kernel 빌드 옵션 또는 버전 변경으로 심볼이 사라져도 agent 가
	// fail-close 되지 않게 한다.
	attachOptionalKprobe("tcp_v4_rcv", objs.HandleTcpV4Rcv, &links)
	attachOptionalKprobe("tcp_v4_do_rcv", objs.HandleTcpV4DoRcv, &links)
	attachOptionalKprobe("tcp_rcv_established", objs.HandleTcpRcvEstablished, &links)
	attachOptionalKprobe("tcp_recvmsg", objs.HandleTcpRecvmsg, &links)

	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		return err
	}
	defer rd.Close()

	go func() {
		<-ctx.Done()
		_ = rd.Close()
	}()

	if onReady != nil {
		onReady(&Runtime{
			PodBytes: objs.PodBytes,
		})
	}

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
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.NativeEndian, &ev); err != nil {
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
