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

	"netobs/internal/gpuobs/metrics"
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

// droppedRefreshInterval은 productionProfiler가 BPF nccl_dropped percpu 카운터와 userspace 채널
// 드롭을 read + sum 해 gpuobs_nccl_events_lost_total에 누적하는 주기다. cuda 패키지의 dropped
// 수집과 동일하게 30s로 둔다.
const droppedRefreshInterval = 30 * time.Second

// productionProfiler는 libnccl.so uprobe 기반 Profiler 구현이다. Attach가 BPF 객체 로드와 심볼
// attach와 ringbuf reader 기동을 수행하고 read loop goroutine이 collective event를 Event 채널로
// emit한다. Close가 read loop 종료와 link / ringbuf / BPF object를 graceful 해제한다.
type productionProfiler struct {
	libPath  string
	nodeName string

	objs   NcclUprobeObjects
	links  []link.Link
	rd     *ringbuf.Reader
	events chan Event

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// closeEvents는 events 채널이 정확히 한 번만 닫히도록 보장한다. read loop가 채널을 닫지 않고
	// Close가 wg.Wait 이후 닫아, Attach 미수행 / attach 실패 / 중복 Close 모든 경로에서 Events 소비자
	// 의 range가 정상 종료하고 double close panic이 없다.
	closeEvents sync.Once

	attached atomic.Bool
	// droppedUserspace는 Event 채널이 가득 차 read loop가 drop한 이벤트 수다. dropped refresher가
	// BPF nccl_dropped와 합산해 gpuobs_nccl_events_lost_total에 누적한다.
	droppedUserspace atomic.Uint64
}

// NewProduction은 libnccl.so.2 절대경로와 노드명으로 production Profiler를 만든다. libPath는
// DaemonSet의 hostPath 마운트 결과 (예: /host/usr/lib/x86_64-linux-gnu/libnccl.so.2) 이고 nodeName은
// gpuobs_nccl_events_lost_total의 라벨이다. 본 시점에는 attach를 수행하지 않으며 Attach 호출 시
// BPF 로드와 심볼 attach가 일어난다.
func NewProduction(libPath, nodeName string) Profiler {
	return &productionProfiler{
		libPath:  libPath,
		nodeName: nodeName,
		events:   make(chan Event, eventChanBuffer),
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

	p.wg.Add(1)
	go p.runDroppedRefresher()

	p.attached.Store(true)
	log.Printf("nccl: attached %d/%d collective symbol pairs on %s", attached, len(probes), p.libPath)
	return nil
}

// readLoop는 ringbuf event를 decode해 Event 채널로 non-blocking send한다. 채널이 가득 차면
// droppedUserspace를 증가시키고 drop해 kernel ringbuf back-pressure를 피한다. ctx 종료 또는
// ringbuf close 시 종료한다. events 채널 close는 Close가 wg.Wait 이후 단독 수행하므로 read loop는
// 채널을 닫지 않는다 (double close 방지).
func (p *productionProfiler) readLoop() {
	defer p.wg.Done()

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

// Close는 read loop와 dropped refresher를 종료시키고 events 채널과 ringbuf reader와 uprobe link와
// BPF object를 graceful 해제한다. cancel과 wg.Wait로 goroutine 종료를 보장한 뒤 events 채널을 단독
// close하므로 read loop가 채널에 send 중인 상태와 겹치지 않는다. Attach 전 호출 / attach 실패 /
// 중복 호출 모든 경로에서 events 채널이 정확히 한 번 닫혀 Events 소비자의 range가 정상 종료한다.
func (p *productionProfiler) Close() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
	p.closeEvents.Do(func() { close(p.events) })
	p.closeLinks()
	if p.attached.Load() {
		_ = p.objs.Close()
	}
	p.attached.Store(false)
	return nil
}

// runDroppedRefresher는 droppedRefreshInterval 주기로 BPF nccl_dropped percpu 카운터와 userspace
// 채널 드롭의 합을 read + sum 해 baseline-then-delta로 gpuobs_nccl_events_lost_total에 누적한다.
// percpu 카운터와 userspace 카운터 모두 단조증가라 delta만 가산하며 BPF map reset 같은 역행 케이스는
// 거짓 spike 회피를 위해 baseline만 갱신하고 skip한다. ctx 종료 시 마지막 1회 수집 후 종료한다.
func (p *productionProfiler) runDroppedRefresher() {
	defer p.wg.Done()

	var baseline uint64
	var initialized bool
	refresh := func() {
		total := readNcclDroppedTotal(p.objs.NcclDropped) + p.droppedUserspace.Load()
		if !initialized {
			baseline = total
			initialized = true
			return
		}
		if total < baseline {
			baseline = total
			return
		}
		if delta := total - baseline; delta > 0 {
			metrics.AddNcclEventsLost(p.nodeName, delta)
			baseline = total
		}
	}

	// 첫 수집을 baseline으로 잡아 attach 이전 누적이 spike로 잡히지 않게 한다.
	refresh()

	t := time.NewTicker(droppedRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			refresh()
			return
		case <-t.C:
			refresh()
		}
	}
}

// readNcclDroppedTotal은 nccl_dropped percpu 카운터를 모든 CPU에 걸쳐 합산한다. cuda 패키지의
// readDroppedTotal과 동일 패턴이다. lookup 실패 시 0을 돌려줘 self-health 수집이 graceful하게
// 진행된다.
func readNcclDroppedTotal(droppedMap *cebpf.Map) uint64 {
	if droppedMap == nil {
		return 0
	}
	var perCPU []uint64
	var key uint32
	if err := droppedMap.Lookup(key, &perCPU); err != nil {
		log.Printf("nccl dropped map lookup: %v", err)
		return 0
	}
	var sum uint64
	for _, v := range perCPU {
		sum += v
	}
	return sum
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
