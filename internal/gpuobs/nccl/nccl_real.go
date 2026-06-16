//go:build nccl

// nccl_real.go는 build tag nccl이 활성인 빌드에서만 컴파일되는 production Profiler 구현이다.
// libnccl.so.2의 collective communication 심볼 (ncclAllReduce와 ncclBroadcast와
// ncclReduceScatter와 ncclAllGather) 에 uprobe와 uretprobe를 attach해 collective의 wall-clock
// 분포를 ringbuf로 수집하고 Event 채널로 emit한다. 본 파일은 cilium/ebpf의 uprobe_multi link와
// ringbuf reader에 의존하므로 NCCL 데이터센터 GPU 환경의 빌드에서만 활성하며, 기본 빌드는
// nccl_stub.go의 NewProduction이 noop을 돌려준다.
//
// bpf2go 산출물 (nccluprobe_bpfel.go / nccluprobe_bpfeb.go) 또한 동일한 build tag nccl로 생성되어
// 기본 빌드는 본 구현과 산출물을 모두 무시한다. attach mechanism은 cuda 패키지와 동일하게
// uprobe_multi link (BPF_LINK_CREATE with BPF_TRACE_UPROBE_MULTI) 를 사용해 perf_event_paranoid
// 정책을 우회하고 CAP_BPF와 CAP_PERFMON과 CAP_SYS_PTRACE만으로 attach가 성립한다.
package nccl

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

// nccl collective op enum. bpf/nccl_uprobe.bpf.c의 enum nccl_op과 1:1 정합해야 한다. 어긋나면
// operationName이 잘못된 라벨을 emit한다.
const (
	opAllReduce     uint8 = 1
	opBroadcast     uint8 = 2
	opReduceScatter uint8 = 3
	opAllGather     uint8 = 4
)

// rawNcclEventSize는 bpf/nccl_uprobe.bpf.c의 struct nccl_event 고정 크기 (32 bytes) 다. ringbuf
// wire 바이트가 본 길이 미만이면 decode를 skip한다.
const rawNcclEventSize = 32

// eventChanBuffer는 productionProfiler가 emit하는 Event 채널 버퍼다. 소비자 (cmd/gpuobs-agent의
// metrics 기록 goroutine) 가 일시적으로 느려도 read loop가 ringbuf를 비우도록 충분히 크게 둔다.
// 버퍼가 가득 차면 read loop가 non-blocking send로 drop해 kernel ringbuf back-pressure를 피한다.
const eventChanBuffer = 4096

// collectiveProbe는 attach 대상 collective 심볼 한 종의 entry와 exit 프로그램 쌍이다. 심볼 단위로
// UprobeMulti와 UretprobeMulti를 호출한다.
type collectiveProbe struct {
	symbol string
	entry  *cebpf.Program
	exit   *cebpf.Program
}

// productionProfiler는 libnccl.so uprobe 기반 Profiler 구현이다. Attach가 BPF 객체 로드와 심볼
// attach와 ringbuf reader 기동을 수행하고 read loop goroutine이 collective event를 Event 채널로
// emit한다. Close가 read loop 종료와 link / ringbuf / BPF object를 graceful 해제한다.
type productionProfiler struct {
	libPath string

	objs   NcclUprobeObjects
	links  []link.Link
	rd     *ringbuf.Reader
	events chan Event

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	attached atomic.Bool
	// droppedUserspace는 Event 채널이 가득 차 read loop가 drop한 이벤트 수다. 소비자 부재 또는
	// 지연의 self-health 신호로 Close 시 1회 로깅한다.
	droppedUserspace atomic.Uint64
}

// NewProduction은 libnccl.so.2 절대경로로 production Profiler를 만든다. libPath는 DaemonSet의
// hostPath 마운트 결과 (예: /host/usr/lib/x86_64-linux-gnu/libnccl.so.2) 다. 본 시점에는 attach를
// 수행하지 않으며 Attach 호출 시 BPF 로드와 심볼 attach가 일어난다.
func NewProduction(libPath string) Profiler {
	return &productionProfiler{
		libPath: libPath,
		events:  make(chan Event, eventChanBuffer),
	}
}

// Available은 Attach가 성공해 read loop가 가동 중이면 true를 돌려준다. uprobe attach는 1회성이라
// Attach 이후 상태가 안정적이며 Close 전까지 true를 유지한다. cmd/gpuobs-agent의 wire-up이 본
// 값을 gpuobs_nccl_profiler_available 게이지에 set한다.
func (p *productionProfiler) Available() bool {
	return p.attached.Load()
}

// Attach는 rlimit memlock 해제 후 BPF 객체를 로드하고 libnccl.so의 collective 심볼에 uprobe와
// uretprobe를 attach한 뒤 ringbuf reader와 read loop goroutine을 기동한다. 모든 심볼 attach가
// 실패하면 진단을 위해 에러로 빠진다. 일부 실패는 warn 로깅 후 attach된 심볼만으로 진행한다.
func (p *productionProfiler) Attach() error {
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock: %w", err)
	}
	if err := LoadNcclUprobeObjects(&p.objs, nil); err != nil {
		return fmt.Errorf("load nccl uprobe objects: %w", err)
	}

	ex, err := link.OpenExecutable(p.libPath)
	if err != nil {
		_ = p.objs.Close()
		return fmt.Errorf("open libnccl %q: %w", p.libPath, err)
	}

	probes := []collectiveProbe{
		{"ncclAllReduce", p.objs.HandleNcclAllreduceEntry, p.objs.HandleNcclAllreduceExit},
		{"ncclBroadcast", p.objs.HandleNcclBroadcastEntry, p.objs.HandleNcclBroadcastExit},
		{"ncclReduceScatter", p.objs.HandleNcclReducescatterEntry, p.objs.HandleNcclReducescatterExit},
		{"ncclAllGather", p.objs.HandleNcclAllgatherEntry, p.objs.HandleNcclAllgatherExit},
	}

	attached := 0
	for _, pr := range probes {
		// entry uprobe와 exit uretprobe는 wall-clock 산정의 페어다. 둘 중 하나라도 실패하면 해당
		// 심볼의 latency가 산정되지 않으므로 페어 단위로 attach 성공을 센다. uprobe_multi link 경로를
		// 써 perf_event_paranoid 정책 차단을 우회한다 (cuda 패키지와 동일).
		el, err := ex.UprobeMulti([]string{pr.symbol}, pr.entry, nil)
		if err != nil {
			log.Printf("nccl uprobe attach %s: %v", pr.symbol, err)
			continue
		}
		xl, err := ex.UretprobeMulti([]string{pr.symbol}, pr.exit, nil)
		if err != nil {
			log.Printf("nccl uretprobe attach %s: %v", pr.symbol, err)
			_ = el.Close()
			continue
		}
		p.links = append(p.links, el, xl)
		attached++
	}

	if attached == 0 {
		p.closeLinks()
		_ = p.objs.Close()
		return fmt.Errorf("no nccl uprobe attached; check libnccl path %q, kernel >= 6.6 (uprobe_multi link), and CAP_BPF/CAP_PERFMON/CAP_SYS_PTRACE", p.libPath)
	}

	rd, err := ringbuf.NewReader(p.objs.NcclEvents)
	if err != nil {
		p.closeLinks()
		_ = p.objs.Close()
		return fmt.Errorf("nccl ringbuf reader: %w", err)
	}
	p.rd = rd

	p.ctx, p.cancel = context.WithCancel(context.Background())
	// ctx 종료 시 ringbuf reader를 닫아 read loop의 blocking Read를 깨운다.
	go func() {
		<-p.ctx.Done()
		_ = p.rd.Close()
	}()

	p.wg.Add(1)
	go p.readLoop()

	p.attached.Store(true)
	log.Printf("nccl: attached %d/%d collective symbol pairs on %s", attached, len(probes), p.libPath)
	return nil
}

// readLoop는 ringbuf event를 decode해 Event 채널로 non-blocking send한다. 채널이 가득 차면
// droppedUserspace를 증가시키고 drop해 kernel ringbuf back-pressure를 피한다. ctx 종료 또는
// ringbuf close 시 채널을 닫고 종료한다.
func (p *productionProfiler) readLoop() {
	defer p.wg.Done()
	defer close(p.events)

	for {
		record, err := p.rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			if p.ctx.Err() != nil {
				return
			}
			// 일시적 read 오류는 로깅 후 계속 진행한다.
			log.Printf("nccl ringbuf read: %v", err)
			continue
		}

		ev, ok := decodeNcclEvent(record.RawSample)
		if !ok {
			log.Printf("short nccl event: got %d bytes want %d", len(record.RawSample), rawNcclEventSize)
			continue
		}

		select {
		case p.events <- ev:
		case <-p.ctx.Done():
			return
		default:
			p.droppedUserspace.Add(1)
		}
	}
}

// Events는 read loop가 emit하는 collective event 채널을 돌려준다. Close 시 채널이 닫혀 소비자의
// range 루프가 정상 종료한다.
func (p *productionProfiler) Events() <-chan Event {
	return p.events
}

// Close는 read loop를 종료시키고 ringbuf reader와 uprobe link와 BPF object를 graceful 해제한다.
// Attach 전 호출 또는 중복 호출에 안전하다.
func (p *productionProfiler) Close() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	if dropped := p.droppedUserspace.Load(); dropped > 0 {
		log.Printf("nccl: dropped %d events due to slow consumer", dropped)
	}
	p.closeLinks()
	if p.attached.Load() {
		_ = p.objs.Close()
	}
	p.attached.Store(false)
	return nil
}

// closeLinks는 attach된 uprobe / uretprobe link를 모두 닫는다.
func (p *productionProfiler) closeLinks() {
	for _, l := range p.links {
		_ = l.Close()
	}
	p.links = nil
}

// decodeNcclEvent는 ringbuf wire 바이트를 Event로 디코드한다. bpf/nccl_uprobe.bpf.c의 struct
// nccl_event layout (32 bytes: ts_ns 8 + duration_ns 8 + pid 4 + tid 4 + rank_count 4 + op 1 +
// pad 3) 과 정확히 일치해야 한다. ts_ns는 monotonic clock이라 wall-clock 변환 없이 무시하고
// Timestamp는 수신 시각으로 채운다. 길이가 부족하면 ok=false를 돌려준다.
func decodeNcclEvent(b []byte) (Event, bool) {
	if len(b) < rawNcclEventSize {
		return Event{}, false
	}
	op := b[28]
	return Event{
		Operation:  operationName(op),
		DurationNs: binary.NativeEndian.Uint64(b[8:16]),
		RankCount:  int(binary.NativeEndian.Uint32(b[24:28])),
		Timestamp:  time.Now(),
	}, true
}

// operationName은 collective op enum을 recording rule과 dashboard에서 쓰는 소문자 라벨로 매핑한다.
// 정의되지 않은 값은 "unknown"으로 폴백해 BPF와 userspace enum drift를 가시화한다.
func operationName(op uint8) string {
	switch op {
	case opAllReduce:
		return "allreduce"
	case opBroadcast:
		return "broadcast"
	case opReduceScatter:
		return "reducescatter"
	case opAllGather:
		return "allgather"
	default:
		return "unknown"
	}
}
