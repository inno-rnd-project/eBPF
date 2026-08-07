package ebpfx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log"
	"net"
	"os"
	"strings"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"

	"netobs/internal/netobs/metrics"
	"netobs/internal/netobs/types"
)

// #105 attach retry 정책. linear backoff 500ms, max retries 3 회, 전체 budget 5s. budget 산정 근거는
// CO-RE relocation 의 driver init 비용 추정 (kernel BTF resolve + verifier 부담 ~500ms × 3) 으로 본 PR
// 의 docs/netobs/bpf-self-health.md 에 근거 정리. 운영 중 dynamic tuning 은 본 이슈 비목표 로 hardcoded.
// `attachRetryBackoff` 와 `attachTotalBudget` 은 var 로 두어 단위 테스트 가 짧은 값 으로 override 후
// 빠르게 retry 흐름 을 검증 가능 하게 한다 (테스트 가 500ms × 3 실제 대기 하면 CI 피드백 루프 가 느려짐).
// `attachMaxRetries` 는 logic invariant (loop 종료 조건) 와 강결합 이라 const 유지.
const attachMaxRetries = 3

var (
	attachRetryBackoff = 500 * time.Millisecond
	attachTotalBudget  = 5 * time.Second
)

// attachWithRetry 는 #105 의 BPF program attach 재시도 진입점 이다. fn closure 는 program type 별
// cilium/ebpf API 차이 (link.Kprobe / link.Kretprobe / link.Tracepoint 등) 를 흡수 하므로 본 helper 가
// program type 무관 으로 재사용 가능 하다. 시도 마다 실패 시 classifyAttachError 결과 를
// metrics.RecordBpfAttachRetry 로 emit 하고, 최종 결과 (success 또는 budget 소진 후 failure) 를
// metrics.RecordBpfAttachResult 로 emit 한다. retry 동안 race-free 보장은 startup 단일 goroutine 흐름
// 이라 추가 동기화 불요.
func attachWithRetry(program string, fn func() (link.Link, error)) (link.Link, error) {
	deadline := time.Now().Add(attachTotalBudget)
	var lastErr error
	for attempt := 0; attempt <= attachMaxRetries; attempt++ {
		l, err := fn()
		if err == nil {
			metrics.RecordBpfAttachResult(program, true)
			return l, nil
		}
		lastErr = err

		// 마지막 시도 였거나 budget 소진 한 경우 retry 중단. counter 가 "retry" 의미 그대로 가 되도록
		// 본 분기 (= retry 미진행) 에서는 counter 증가 하지 않고 break.
		if attempt == attachMaxRetries || time.Now().Add(attachRetryBackoff).After(deadline) {
			break
		}
		// 실제 retry 가 일어날 때만 reason 분류 후 retry counter +1. 마지막 실패는 retry 가 아니라
		// failed attempt 라 attach_total{result="failure"} 에만 반영 한다.
		reason := classifyAttachError(err)
		metrics.RecordBpfAttachRetry(program, reason.String())
		time.Sleep(attachRetryBackoff)
	}
	metrics.RecordBpfAttachResult(program, false)
	return nil, lastErr
}

// fakeAttachSymbols 는 #105 verify.sh 의 시뮬 진입점 이다. `NETOBS_BPF_FAKE_ATTACH_SYMBOLS` env 가
// 명시 되면 본 env 의 콤마 구분 symbol 들 을 trackedSymbols 와 동일 라이프사이클 로 attach 시도 한다.
// fake symbol 은 kernel 에 존재 하지 않 으므로 attach 가 자연 실패 (syscall.ENOENT → symbol_not_found)
// 해 attach_total{result="failure"} 와 attach_retry_total{reason="symbol_not_found"} 메트릭 발화 를 통해
// e2e verify.sh 가 attach 실패 경로 회귀 가드 를 수행 가능 하게 한다. prod overlay 미설정 으로 자연 차단.
func fakeAttachSymbols() []string {
	raw := strings.TrimSpace(os.Getenv("NETOBS_BPF_FAKE_ATTACH_SYMBOLS"))
	if raw == "" {
		return nil
	}
	out := make([]string, 0)
	for _, tok := range strings.Split(raw, ",") {
		if sym := strings.TrimSpace(tok); sym != "" {
			out = append(out, sym)
		}
	}
	return out
}

// trackedSymbols 는 netlat BPF 가 attach 시도할 kprobe / kretprobe 심볼 21 종이다. required 2 종 +
// optional 19 종으로 구성되며 슬라이스 순서는 Run 의 attach 루프 순서와 정합한다. gpuobs 의
// trackedSymbols (cuda loader.go:29) 와 동일하게 metrics.SetBpfProgramLoaded 의 라벨 cardinality
// 를 폐쇄적으로 잡아 attach 단계 이전에도 모든 심볼이 0 으로 선등록되도록 한다.
// #103 IPv6 TCP receive path 2 종 (tcp_v6_rcv, tcp_v6_do_rcv) 과 UDP TX/RX probe 4 종 추가.
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
	// #197 수신측 ACK 대기 (ack_wait) stage. tcp_send_ack 가 standalone ACK 송신 시점 에 대기 latency 를
	// emit 한다. 다른 심볼 과 동일 라이프사이클 로 metrics.SetBpfProgramLoaded 에 0 으로 선등록 되게 한다.
	"tcp_send_ack",
	// #227 client 측 TCP 연결 수립 지연 (connect stage) 3 종.
	"tcp_v4_connect",
	"tcp_v6_connect",
	"tcp_finish_connect",
	// #103 UDP TX/RX probe 4 종. connected UDP 만 추적 (sk_state==TCP_ESTABLISHED).
	"udp_sendmsg",
	"udp_recvmsg",
	"udpv6_sendmsg",
	"udpv6_recvmsg",
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
	l, err := attachWithRetry(symbol, func() (link.Link, error) {
		return link.Kprobe(symbol, prog, nil)
	})
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
	program := symbol + "_ret"
	l, err := attachWithRetry(program, func() (link.Link, error) {
		return link.Kretprobe(symbol, prog, nil)
	})
	if err != nil {
		return err
	}
	*links = append(*links, l)
	metrics.SetBpfProgramLoaded(program, true)
	log.Printf("attached kretprobe/%s", symbol)
	return nil
}

func attachOptionalKprobe(symbol string, prog *cebpf.Program, links *[]link.Link) {
	l, err := attachWithRetry(symbol, func() (link.Link, error) {
		return link.Kprobe(symbol, prog, nil)
	})
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
	program := symbol + "_ret"
	l, err := attachWithRetry(program, func() (link.Link, error) {
		return link.Kretprobe(symbol, prog, nil)
	})
	if err != nil {
		log.Printf("skip optional kretprobe/%s: %v", symbol, err)
		return
	}
	*links = append(*links, l)
	metrics.SetBpfProgramLoaded(program, true)
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
	// FlowBytes 는 #85 의 BPF_MAP_TYPE_LRU_HASH 맵 (max_entries=131072, #351/#403 실측 기반 상향) 이다. tcp_sendmsg_ret 과
	// tcp_cleanup_rbuf 의 inc_flow_bytes 호출 이 5-tuple key 로 본 맵 에 누적 한다. userspace flow.
	// Collector 가 scrape 시점 에 본 맵 을 iterate 해 netobs_flow_bytes_total counter 로 emit 한다.
	FlowBytes *cebpf.Map
	// SegAccum / RecvStarts / ConnectStarts 는 #413 의 utilization 편입 대상 LRU 맵 3종이다.
	// self-health refresher 가 entry 수를 iterate 해 netobs_bpf_map_utilization_ratio 로 emit 하며,
	// LRU evict 로 인한 표본 누락 (재전송 seq 추적 / 수신·연결 타이머 유실) 이 무증상으로 진행되지
	// 않게 한다. nic_ingress 는 1-entry PERCPU_ARRAY 라 편입 대상이 아니다 (#416).
	SegAccum      *cebpf.Map
	RecvStarts    *cebpf.Map
	ConnectStarts *cebpf.Map
}

// Run은 BPF 오브젝트 로드, 프로브 attach, ringbuf reader 준비가 모두 끝난 시점에
// onReady를 호출해 상위에 readiness를 알리고 Runtime 핸들을 넘긴다. onReady가 nil이면 무시한다.
func Run(ctx context.Context, targetIP string, out chan<- types.Event, onReady func(*Runtime)) error {
	defer close(out)

	// 진단 시그널 일관성: attach 시도 자체가 일어나기 전에 모든 심볼을 0 으로 선등록해 둔다.
	// LoadNetObsObjects 실패 / capability 부족 등으로 attach 단계조차 못 가는 상황도 운영자가
	// netobs_bpf_program_loaded 메트릭만 보고 진단할 수 있다 (gpuobs cuda loader 의 동일 패턴).
	allSymbols := append([]string(nil), trackedSymbols...)
	if fake := fakeAttachSymbols(); len(fake) > 0 {
		// #105 verify.sh 시뮬 진입점. fake symbol 은 kernel 부재 라 attach 시도 시 ENOENT 자연 실패 →
		// attach_total{result="failure"} 와 attach_retry_total{reason="symbol_not_found"} 메트릭 발화.
		log.Printf("netobs: NETOBS_BPF_FAKE_ATTACH_SYMBOLS active: %v (test-only)", fake)
		allSymbols = append(allSymbols, fake...)
	}
	for _, sym := range allSymbols {
		metrics.SetBpfProgramLoaded(sym, false)
	}

	// #105 attach 카운터 라벨 사전 등록. tracked + fake program 셋과 7종 reason enum 의 모든 조합을
	// 0 으로 노출 해 attach 시도 전 / 정상 환경 모두에서 dashboard query 가 empty 가 아닌 0 시계열을
	// 받게 한다. reasons 는 caller (본 패키지) 가 AttachReasonValues 를 String() 변환 후 전달.
	reasonStrings := make([]string, 0, len(AttachReasonValues))
	for _, r := range AttachReasonValues {
		reasonStrings = append(reasonStrings, r.String())
	}
	// kretprobe 라벨은 "_ret" 접미가 부착되므로 program 라벨 셋에 함께 포함.
	preregPrograms := make([]string, 0, len(allSymbols))
	preregPrograms = append(preregPrograms, allSymbols...)
	metrics.PreregisterBpfAttachLabels(preregPrograms, reasonStrings)

	if err := rlimit.RemoveMemlock(); err != nil {
		return err
	}

	var objs NetObsObjects
	if err := LoadNetObsObjects(&objs, nil); err != nil {
		return err
	}
	defer func() { _ = objs.Close() }()

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

	// #173 NIC ingress→L3 stage 의 진입점. __netif_receive_skb 는 NIC 드라이버가 skb 를 커널 stack 에
	// 올리는 보편 진입점으로, 공개 심볼 netif_receive_skb 가 NAPI / backlog 경로에 우회되어 거의
	// 발화하지 않는 점을 dev 실측으로 확인해 본 심볼을 hook 한다. softirq context 라 socket 미해상
	// 이므로 진입 시각만 per-CPU stash 하고 Pod 귀속과 emit 은 tcp_v4_rcv (L3 진입) 으로 미룬다.
	attachOptionalKprobe("__netif_receive_skb", objs.HandleNetifReceiveSkb, &links)

	// #65 receive path 의 BPF kprobe 4 종. tcp_v4_do_rcv / tcp_rcv_established / tcp_recvmsg 3 종은
	// sock 인자가 있어 emit_rcv_event 로 stage 별 진입 시점과 TCP 상태를 ringbuf 에 emit 하고,
	// tcp_v4_rcv 는 socket lookup 이전이지만 #173 부터 skb->sk (early demux) 로 NIC ingress→L3 (RCV_NIC)
	// stage 를 emit 한다 (RCV_L3 자체 emit 은 보류 유지). attachOptionalKprobe 를 사용해 kernel 빌드
	// 옵션 또는 버전 변경으로 심볼이 사라져도 agent 가 fail-close 되지 않게 한다.
	attachOptionalKprobe("tcp_v4_rcv", objs.HandleTcpV4Rcv, &links)
	attachOptionalKprobe("tcp_v4_do_rcv", objs.HandleTcpV4DoRcv, &links)
	attachOptionalKprobe("tcp_rcv_established", objs.HandleTcpRcvEstablished, &links)
	attachOptionalKprobe("tcp_recvmsg", objs.HandleTcpRecvmsg, &links)

	// #197 수신측 ACK 대기 (ACK_WAIT) stage. tcp_rcv_established 가 stash 한 첫 미-ACK 데이터 수신 시각과
	// tcp_send_ack (지연 ACK / quickack standalone ACK 송신 지점) 의 차분을 emit 한다. family 무관 단일
	// 함수라 IPv4/IPv6 흐름을 함께 capture 하며, 심볼 부재 시 fail-close 되지 않게 optional attach 한다.
	attachOptionalKprobe("tcp_send_ack", objs.HandleTcpSendAck, &links)

	// #227 client 측 TCP 연결 수립 지연 (CONNECT) stage. v4 / v6 connect 진입이 stash 하고 공용
	// tcp_finish_connect 가 emit 한다. 심볼 부재 시 fail-close 없이 skip 된다.
	attachOptionalKprobe("tcp_v4_connect", objs.HandleTcpV4Connect, &links)
	attachOptionalKprobe("tcp_v6_connect", objs.HandleTcpV6Connect, &links)
	attachOptionalKprobe("tcp_finish_connect", objs.HandleTcpFinishConnect, &links)

	// #103 IPv6 TCP receive path attach. tcp_v6_rcv 는 stub (cgroup 미식별), tcp_v6_do_rcv 는 sock
	// 기반 demux event 를 emit 한다. tcp_rcv_established 와 tcp_recvmsg 는 family 무관 단일 함수 라
	// 이미 IPv4 attach 가 IPv6 흐름 도 함께 capture 한다 (c2 의 emit_rcv_event 가 family 분기 처리).
	attachOptionalKprobe("tcp_v6_rcv", objs.HandleTcpV6Rcv, &links)
	attachOptionalKprobe("tcp_v6_do_rcv", objs.HandleTcpV6DoRcv, &links)

	// #103 UDP TX/RX 4 hook attach. connected UDP 만 추적 (handle_udp_msg 의 sk_state 가드). IPv4 /
	// IPv6 양쪽 family 처리.
	attachOptionalKprobe("udp_sendmsg", objs.HandleUdpSendmsg, &links)
	attachOptionalKprobe("udp_recvmsg", objs.HandleUdpRecvmsg, &links)
	attachOptionalKprobe("udpv6_sendmsg", objs.HandleUdpv6Sendmsg, &links)
	attachOptionalKprobe("udpv6_recvmsg", objs.HandleUdpv6Recvmsg, &links)

	// #105 fake symbol attach 시뮬. NETOBS_BPF_FAKE_ATTACH_SYMBOLS env 명시 시에만 진입. 본 경로는
	// kernel 부재 symbol 이라 attach 자연 실패 → attach_total{result="failure"} 와 attach_retry_total
	// {reason="symbol_not_found"} 메트릭 발화 가 e2e verify.sh 의 회귀 가드 진입점 으로 동작 한다.
	// 실제 BPF program 으로는 HandleTcpSendmsg 를 재사용 (program 자체는 무관, attach 자체가 실패).
	for _, sym := range fakeAttachSymbols() {
		attachOptionalKprobe(sym, objs.HandleTcpSendmsg, &links)
	}

	rd, err := ringbuf.NewReader(objs.Events)
	if err != nil {
		return err
	}
	defer func() { _ = rd.Close() }()

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
			SegAccum:      objs.SegAccum,
			RecvStarts:    objs.RecvStarts,
			ConnectStarts: objs.ConnectStarts,
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
