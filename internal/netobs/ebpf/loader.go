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

// trackedSymbols 는 netlat BPF 가 attach 시도할 kprobe / kretprobe 심볼 17 종이다. required 2 종 +
// optional 15 종으로 구성되며 슬라이스 순서는 Run 의 attach 루프 순서와 정합한다. gpuobs 의
// trackedSymbols (cuda loader.go:29) 와 동일하게 metrics.SetBpfProgramLoaded 의 라벨 cardinality
// 를 폐쇄적으로 잡아 attach 단계 이전에도 모든 심볼이 0 으로 선등록되도록 한다.
// #103 IPv6 TCP receive path 2 종 (tcp_v6_rcv, tcp_v6_do_rcv) 추가.
var trackedSymbols = []string{
	"tcp_sendmsg",
	"tcp_sendmsg_ret",
	// #82 send path stage 4 분해의 kernel 함수. tcp_write_xmit 는 TCP control path, __tcp_transmit_skb
	// 는 개별 segment transmit entry 다. 두 함수 모두 kernel 6.x 에서 stable kprobe 가능 심볼이다.
	"tcp_write_xmit",
	"tcp_write_xmit_ret",
	"__tcp_transmit_skb",
	"__tcp_transmit_skb_ret",
	"veth_xmit",
	"__dev_queue_xmit",
	"tcp_retransmit_skb",
	"kfree_skb_reason",
	"tcp_cleanup_rbuf",
	"tcp_v4_rcv",
	"tcp_v4_do_rcv",
	"tcp_v6_rcv",
	"tcp_v6_do_rcv",
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

// attachOptionalKretprobe 는 attachOptionalKprobe 와 동일 패턴의 kretprobe 버전이다. metrics
// 측 라벨은 attachRequiredKretprobe 와 마찬가지로 symbol 에 "_ret" 접미사를 붙여 kprobe 와
// 구분되도록 한다. trackedSymbols 의 "*_ret" 항목과 정합한다.
func attachOptionalKretprobe(symbol string, prog *cebpf.Program, links *[]link.Link) {
	l, err := link.Kretprobe(symbol, prog, nil)
	if err != nil {
		log.Printf("skip optional kretprobe/%s: %v", symbol, err)
		return
	}
	*links = append(*links, l)
	metrics.SetBpfProgramLoaded(symbol+"_ret", true)
	log.Printf("attached kretprobe/%s", symbol)
}

// Runtime은 Run이 onReady 콜백으로 상위에 넘기는 BPF 런타임 핸들이다. ringbuf events는 채널로 따로
// 흘러가므로 Runtime에는 외부 컴포넌트가 scrape 등 다른 lifecycle에서 직접 읽어야 하는 BPF 맵만 노출한다.
type Runtime struct {
	// PodBytes는 (cgroup_id, direction, layer) 키로 누적되는 LRU PERCPU HASH 맵으로,
	// podbytes collector가 scrape 시점에 iterate해 Prometheus counter로 emit한다.
	PodBytes *cebpf.Map
	// Starts 는 (tid → netobs_start_info) 의 LRU_HASH 맵이다. self-health refresher 가
	// 30s 주기 entry 수 iterate 로 netobs_bpf_map_utilization_ratio 를 산정할 때 참조한다.
	Starts *cebpf.Map
	// EventsDropped 는 events ringbuf 의 reserve 실패 percpu counter (BPF_MAP_TYPE_PERCPU_ARRAY,
	// max_entries=1) 다. self-health refresher 가 baseline-then-delta 로 본 카운터를 읽어
	// netobs_bpf_ringbuf_drops_total 에 누적한다.
	EventsDropped *cebpf.Map
	// DropStacks 는 #83 의 BPF_MAP_TYPE_STACK_TRACE 맵 (max_entries=10240) 이다. handle_kfree_skb_
	// reason 의 bpf_get_stackid 가 본 맵에 stack frame 배열을 적재하고 stack id 를 ringbuf event 에
	// carry 한다. userspace symbol resolver 가 본 맵을 Lookup(stack_id) 으로 조회해 IP 배열을 얻고
	// kallsyms 로 frame 별 함수명을 산정한다.
	DropStacks *cebpf.Map
	// FlowBytes 는 #85 의 BPF_MAP_TYPE_LRU_HASH 맵 (max_entries=1024) 이다. tcp_sendmsg_ret 과
	// tcp_cleanup_rbuf 의 inc_flow_bytes 호출 이 5-tuple key 로 본 맵 에 누적 한다. userspace flow.
	// Collector 가 scrape 시점 에 본 맵 을 iterate 해 netobs_flow_bytes_total counter 로 emit 한다.
	FlowBytes *cebpf.Map
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

	// #82 send path stage 4 분해. tcp_write_xmit 은 TCP control path (cwnd / nagle / window throttle)
	// latency, __tcp_transmit_skb 는 개별 segment transmit latency 를 측정한다. attachOptional
	// 패턴이라 kernel 빌드 옵션 또는 버전 변경으로 심볼이 사라져도 agent 가 fail-close 되지 않게 한다.
	attachOptionalKprobe("tcp_write_xmit", objs.HandleTcpWriteXmit, &links)
	attachOptionalKretprobe("tcp_write_xmit", objs.HandleTcpWriteXmitRet, &links)
	attachOptionalKprobe("__tcp_transmit_skb", objs.HandleTcpTransmitSkb, &links)
	attachOptionalKretprobe("__tcp_transmit_skb", objs.HandleTcpTransmitSkbRet, &links)

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

	// #103 IPv6 TCP receive path attach. tcp_v6_rcv 는 stub (cgroup 미식별), tcp_v6_do_rcv 는 sock
	// 기반 demux event 를 emit 한다. tcp_rcv_established 와 tcp_recvmsg 는 family 무관 단일 함수 라
	// 이미 IPv4 attach 가 IPv6 흐름 도 함께 capture 한다 (c2 의 emit_rcv_event 가 family 분기 처리).
	attachOptionalKprobe("tcp_v6_rcv", objs.HandleTcpV6Rcv, &links)
	attachOptionalKprobe("tcp_v6_do_rcv", objs.HandleTcpV6DoRcv, &links)

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
			PodBytes:      objs.PodBytes,
			Starts:        objs.Starts,
			EventsDropped: objs.EventsDropped,
			DropStacks:    objs.DropStacks,
			FlowBytes:     objs.FlowBytes,
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
