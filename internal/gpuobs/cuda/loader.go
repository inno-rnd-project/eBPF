package cuda

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	cebpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"

	"netobs/internal/gpuobs/metrics"
	"netobs/internal/gpuobs/nvml"
	"netobs/internal/gpuobs/types"
	"netobs/internal/kube"
)

// PodResolver 는 cuda 패키지가 PID → PodIdentity 해석에 의존하는 최소 인터페이스다.
// 운영에서는 *kube.Resolver 가 자연스럽게 만족하며, 테스트에서는 fake 로 주입한다.
type PodResolver interface {
	ResolvePID(pid uint32) kube.PodIdentity
}

// trackedSymbols 는 libcuda.so 에 attach 시도할 심볼 14종이다. 슬라이스 순서는 Reader.Run 의
// attach 루프와 metrics 의 symbol_available 라벨에 그대로 노출된다.
//
// 추가 심볼은 BPF 측 SEC 정의와 progBySymbol 매핑을 함께 동시에 갱신해야 한다.
var trackedSymbols = []string{
	"cuLaunchKernel",
	"cuLaunchKernelEx",
	"cuLaunchCooperativeKernel",
	"cuMemcpyHtoD_v2",
	"cuMemcpyHtoDAsync_v2",
	"cuMemcpyDtoH_v2",
	"cuMemcpyDtoHAsync_v2",
	"cuMemcpyDtoD_v2",
	"cuMemcpyDtoDAsync_v2",
	"cuMemcpy2D_v2",
	"cuMemcpy2DAsync_v2",
	"cuMemcpy3D_v2",
	"cuMemcpy3DAsync_v2",
	"cuMemcpy",
}

// cudartTrackedSymbols 는 libcudart.so 에 attach 시도할 CUDA Runtime API 심볼 3종이다.
// libcuda 와 분리된 OpenExecutable 로 처리되며, libcudartPath 가 빈 값이면 본 attach 자체가 skip 된다.
var cudartTrackedSymbols = []string{
	"cudaLaunchKernel",
	"cudaMemcpy",
	"cudaMemcpyAsync",
}

// Reader 는 cuda uprobe 가 emit 한 ringbuf 이벤트의 소비자다. lifecycle 은 Run 이 소유한다.
type Reader struct {
	libcudaPath   string
	libcudartPath string
	nodeName      string
	nv            nvml.NVML
	resolver      PodResolver
	refreshEvery  time.Duration

	// pods 는 PID 를 kube.PodIdentity 로 캐시하는 podMap 이다. dispatch hot path 가 매 이벤트
	// /proc/<pid>/cgroup parse 를 호출하지 않도록 lookup 우선, miss 시 ResolvePID 후 store 한다.
	// runDeviceMapRefresher 가 NVML refresh 사이클마다 active PID 셋 기반으로 통째 교체한다.
	pods *podMap

	// recordEvent 는 metrics.RecordCudaEvent 를 위한 test seam 이다.
	// 운영 코드는 New 에서 metrics.RecordCudaEvent 를 기본값으로 받고, 단위 테스트에서는
	// spy 함수로 교체해 dispatch 가 산출한 sample 을 검증한다.
	recordEvent func(node string, sample metrics.CudaEventSample)
}

// New 는 Reader 를 생성한다.
//   - libcudaPath:   host 의 libcuda.so.1 절대경로. DaemonSet hostPath 마운트 결과 (예: /host/usr/lib/x86_64-linux-gnu/libcuda.so.1).
//   - libcudartPath: host 의 libcudart.so 절대경로. 빈 문자열이면 cudart attach 자체를 skip 한다.
//   - nodeName:      metric 라벨 / log 컨텍스트 용 노드명.
//   - nv:            NVML 핸들. nil 이면 device map refresher 가 즉시 종료해 모든 이벤트가 gpu_uuid="unknown" 으로 발행된다.
//   - resolver:      PID→Pod 해상도 제공자. nil 이면 모든 이벤트가 비-Pod 로 분류되어 metrics 측에서 발행 skip 된다.
//   - refreshEvery:  device map refresh 주기. 0 이하 값은 호출자가 사전 검증해야 한다.
func New(libcudaPath, libcudartPath, nodeName string, nv nvml.NVML, resolver PodResolver, refreshEvery time.Duration) *Reader {
	return &Reader{
		libcudaPath:   libcudaPath,
		libcudartPath: libcudartPath,
		nodeName:      nodeName,
		nv:            nv,
		resolver:      resolver,
		refreshEvery:  refreshEvery,
		pods:          newPodMap(),
		recordEvent:   metrics.RecordCudaEvent,
	}
}

// Run 은 BPF 객체 로드 → libcuda 심볼 attach → device map refresher 기동 → ringbuf 소비 의
// 풀 lifecycle 을 수행한다. ctx 종료 시 모든 uprobe link / ringbuf reader / device handle /
// BPF object 를 graceful 해제한 뒤 nil 을 반환한다.
//
// 모든 7 심볼 attach 가 실패하면 진단을 명확히 하기 위해 즉시 에러로 빠진다. 일부 실패 시에는
// metrics symbol_available 로 경고를 노출하고 attach 된 심볼만으로 계속 진행한다.
func (r *Reader) Run(ctx context.Context, onReady func()) error {
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("remove memlock: %w", err)
	}

	var objs CudaUprobeObjects
	if err := LoadCudaUprobeObjects(&objs, nil); err != nil {
		return fmt.Errorf("load cuda uprobe objects: %w", err)
	}
	defer objs.Close()

	progBySymbol := map[string]*cebpf.Program{
		"cuLaunchKernel":            objs.HandleCuLaunchKernel,
		"cuLaunchKernelEx":          objs.HandleCuLaunchKernelEx,
		"cuLaunchCooperativeKernel": objs.HandleCuLaunchCooperativeKernel,
		"cuMemcpyHtoD_v2":           objs.HandleCuMemcpyHtod,
		"cuMemcpyHtoDAsync_v2":      objs.HandleCuMemcpyHtodAsync,
		"cuMemcpyDtoH_v2":           objs.HandleCuMemcpyDtoh,
		"cuMemcpyDtoHAsync_v2":      objs.HandleCuMemcpyDtohAsync,
		"cuMemcpyDtoD_v2":           objs.HandleCuMemcpyDtod,
		"cuMemcpyDtoDAsync_v2":      objs.HandleCuMemcpyDtodAsync,
		"cuMemcpy2D_v2":             objs.HandleCuMemcpy2d,
		"cuMemcpy2DAsync_v2":        objs.HandleCuMemcpy2dAsync,
		"cuMemcpy3D_v2":             objs.HandleCuMemcpy3d,
		"cuMemcpy3DAsync_v2":        objs.HandleCuMemcpy3dAsync,
		"cuMemcpy":                  objs.HandleCuMemcpy,
	}

	// libcudart 의 program 매핑은 libcudart 경로가 비어 있어도 항상 노출되며, 실제 attach 는 libcudartPath
	// 가 비어있지 않을 때만 수행된다.
	cudartProgBySymbol := map[string]*cebpf.Program{
		"cudaLaunchKernel": objs.HandleCudaLaunchKernel,
		"cudaMemcpy":       objs.HandleCudaMemcpy,
		"cudaMemcpyAsync":  objs.HandleCudaMemcpyAsync,
	}

	// 진단 시그널 일관성: attach 시도 자체가 일어나기 전에 모든 심볼을 0 으로 선등록해 둔다.
	// 이렇게 하면 OpenExecutable 실패 / BPF object 로드 실패 / capability 부족 등으로 attach 단계조차
	// 못 가는 상황도 운영자가 gpuobs_cuda_symbol_available 메트릭만 보고 진단할 수 있다.
	// 성공한 심볼만 이후 1 로 덮어쓴다.
	for _, sym := range trackedSymbols {
		metrics.SetCudaSymbolAvailability(r.nodeName, sym, false)
	}
	for _, sym := range cudartTrackedSymbols {
		metrics.SetCudaSymbolAvailability(r.nodeName, sym, false)
	}

	ex, err := link.OpenExecutable(r.libcudaPath)
	if err != nil {
		return fmt.Errorf("open libcuda %q: %w", r.libcudaPath, err)
	}

	var links []link.Link
	defer func() {
		for _, l := range links {
			_ = l.Close()
		}
	}()

	attached := 0
	for _, sym := range trackedSymbols {
		prog, ok := progBySymbol[sym]
		if !ok || prog == nil {
			// trackedSymbols 에는 있는데 BPF 산출물에 매칭 program 이 없는 케이스 (개발자 실수).
			// 0 은 이미 선등록되어 있으므로 로그만 남기고 계속 진행한다.
			log.Printf("cuda uprobe attach %s: missing ebpf program mapping", sym)
			continue
		}
		l, err := ex.Uprobe(sym, prog, nil)
		if err != nil {
			log.Printf("cuda uprobe attach %s: %v", sym, err)
			continue
		}
		links = append(links, l)
		metrics.SetCudaSymbolAvailability(r.nodeName, sym, true)
		attached++
	}
	if attached == 0 {
		return fmt.Errorf("no cuda uprobe attached; check libcuda path %q and CAP_BPF/CAP_PERFMON/CAP_SYS_PTRACE", r.libcudaPath)
	}
	log.Printf("cuda uprobe attached %d/%d libcuda symbols on %s", attached, len(trackedSymbols), r.libcudaPath)

	// libcudart 는 NVIDIA driver 가 아닌 CUDA Toolkit 의 일부라 host 에 설치된 환경에서만 의미가 있다.
	// libcudartPath 가 빈 값이면 cudart attach 자체를 skip 하고 모든 cudart 심볼은 availability=0 으로 둔다.
	// libcudartPath 가 지정되었더라도 OpenExecutable 실패 (파일 부재 등) 는 fatal 로 다루지 않고 warn 로깅 후 진행한다.
	if r.libcudartPath != "" {
		cudartEx, err := link.OpenExecutable(r.libcudartPath)
		if err != nil {
			log.Printf("cuda uprobe open libcudart %q: %v (cudart symbols remain unavailable)", r.libcudartPath, err)
		} else {
			cudartAttached := 0
			for _, sym := range cudartTrackedSymbols {
				prog, ok := cudartProgBySymbol[sym]
				if !ok || prog == nil {
					log.Printf("cuda uprobe attach %s: missing ebpf program mapping", sym)
					continue
				}
				l, err := cudartEx.Uprobe(sym, prog, nil)
				if err != nil {
					log.Printf("cuda uprobe attach %s: %v", sym, err)
					continue
				}
				links = append(links, l)
				metrics.SetCudaSymbolAvailability(r.nodeName, sym, true)
				cudartAttached++
			}
			log.Printf("cuda uprobe attached %d/%d libcudart symbols on %s", cudartAttached, len(cudartTrackedSymbols), r.libcudartPath)
		}
	}

	rd, err := ringbuf.NewReader(objs.CudaEvents)
	if err != nil {
		return fmt.Errorf("ringbuf reader: %w", err)
	}
	defer rd.Close()

	// runCtx 는 reader / device map refresher 두 goroutine 의 공유 종료 시그널이다.
	// Run 이 어떤 경로로 빠지든 (ctx Done / 에러 return / 정상 종료) defer cancel 이 모두를 깨운다.
	runCtx, cancel := context.WithCancel(ctx)

	// defer 등록 순서: wg.Wait 먼저 등록(LIFO 로 마지막 실행), cancel 나중 등록(먼저 실행).
	// → cancel() → goroutine 들이 종료 → wg.Wait() 완료. 이 순서가 깨지면 Wait 이 영원히 대기한다.
	devmap := newDeviceMap()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		r.runDeviceMapRefresher(runCtx, devmap, objs.CudaDropped)
	}()

	go func() {
		<-runCtx.Done()
		_ = rd.Close()
	}()

	if onReady != nil {
		onReady()
	}

	for {
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return nil
			}
			if runCtx.Err() != nil {
				return nil
			}
			return err
		}

		raw, ok := decodeRawEvent(record.RawSample)
		if !ok {
			log.Printf("short cuda event: got %d bytes want %d", len(record.RawSample), rawEventSize)
			continue
		}

		r.dispatch(raw, devmap)
	}
}

// decodeRawEvent 는 ringbuf wire 바이트를 rawEvent 로 zero-alloc 디코드한다.
// binary.Read + bytes.NewReader 가 reflection 기반이라 hot path 비용이 무시할 수 없어 직접 인덱싱한다.
// 길이가 부족하면 ok=false 를 반환해 호출자가 skip 하도록 한다.
func decodeRawEvent(b []byte) (rawEvent, bool) {
	if len(b) < rawEventSize {
		return rawEvent{}, false
	}
	return rawEvent{
		TsNs:  binary.NativeEndian.Uint64(b[0:8]),
		Bytes: binary.NativeEndian.Uint64(b[8:16]),
		PID:   binary.NativeEndian.Uint32(b[16:20]),
		TID:   binary.NativeEndian.Uint32(b[20:24]),
		Kind:  b[24],
	}, true
}

// dispatch 는 한 raw 이벤트를 PID → PodIdentity / PID → GPU UUID 까지 해상도하고,
// metrics.RecordCudaEvent (또는 test seam recordEvent) 로 누적시킨다.
// 비-Pod 식별자 / 정의되지 않은 kind / 미해상도 GPU UUID 폴백은 모두 metrics 계층에서 흡수된다.
func (r *Reader) dispatch(raw rawEvent, devmap *deviceMap) {
	var id kube.PodIdentity
	if r.resolver != nil {
		// hot path: 캐시 hit 가 일반적이며 RLock 한 번으로 끝난다. miss 인 경우에만 ResolvePID
		// (cgroup read + parse) 를 거쳐 결과를 즉석 store 한다. negative result (비-Pod) 도 적재해
		// 동일 PID 에 대한 ResolvePID 호출이 한 번을 넘지 않게 한다.
		if cached, ok := r.pods.lookup(raw.PID); ok {
			id = cached
		} else {
			id = r.resolver.ResolvePID(raw.PID)
			r.pods.store(raw.PID, id)
		}
	}
	r.recordEvent(r.nodeName, metrics.CudaEventSample{
		ID:      id,
		GPUUUID: devmap.lookup(raw.PID),
		Kind:    types.CudaEventKind(raw.Kind),
		Bytes:   raw.Bytes,
	})
}

// runDeviceMapRefresher 는 r.refreshEvery 주기로 NVML RunningProcesses 를 모든 device 에서
// 모은 뒤 deviceMap 을 atomic replace 하고, 같은 사이클에서 RetainCudaSeries 호출로 종료된
// (Pod, GPU) 의 metric 시리즈를 surgical Delete 한다. 같은 ticker 에서 BPF percpu cuda_dropped
// 카운터도 read + sum 해 baseline-then-delta 로 gpuobs_cuda_events_lost_total 에 누적시킨다.
//
// device 목록은 nvml.DeviceSet 으로 매 사이클마다 Sync 되어 hot-add / hot-remove / index 재배치를
// 자동 흡수한다 (#34). ctx 종료 시 DeviceSet.Close 가 모든 device handle 을 일괄 해제한다.
//
// nv 가 nil 이면 즉시 종료한다 — main 단에서 nv==nil + cuda enabled 시 cuda reader 자체를 시작하지
// 않도록 가드되어 있어 정상 경로에서는 도달하지 않는 안전망이다.
func (r *Reader) runDeviceMapRefresher(ctx context.Context, devmap *deviceMap, droppedMap *cebpf.Map) {
	if r.nv == nil {
		return
	}

	devSet := nvml.NewDeviceSet(r.nv)
	defer func() {
		if err := devSet.Close(); err != nil {
			log.Printf("cuda: device set close: %v", err)
		}
	}()

	// dropped counter 는 metrics 측 ECC / Violation / Energy / PcieReplay 와 동일한 baseline-then-delta
	// 패턴으로 처리한다. 첫 refresh() 시점에 BPF percpu map 이 0 이 아닐 가능성 (cuda reader 가 attach 보다
	// 먼저 map 을 만들고 다른 프로세스가 그 사이 reserve 실패한 케이스 / agent 재시작 시 map 잔존 등) 이
	// 있으므로, 첫 호출은 baseline 만 저장하고 delta 가산은 두 번째 호출부터 시작한다.
	var (
		lastDroppedTotal uint64
		droppedBaselined bool
	)
	refresh := func() {
		// 매 cycle 에 device 셋을 동기화해 hot-plug 변화를 흡수한다. Sync 실패는 warn 로그만 남기고
		// 직전까지의 device 슬라이스를 그대로 사용한다 (다음 cycle 에서 재시도).
		if err := devSet.Sync(); err != nil {
			log.Printf("cuda: device sync: %v", err)
		}
		fresh, multiGPUCount := r.collectPidToUUID(devSet.Snapshot())
		metrics.SetCudaPidMultiGPUCount(r.nodeName, multiGPUCount)
		// active PID 셋에 대해 ResolvePID 를 본 사이클에서 한 번씩만 호출해 podMap 을 통째 교체한다.
		// dispatch hot path 는 다음 사이클까지의 모든 이벤트를 캐시 hit 로 처리하고, 종료된 PID 는
		// 본 replace 로 자연스럽게 제거되어 별도 cleanup 호출이 필요하지 않다.
		r.pods.replace(r.resolvePidToPod(fresh))
		devmap.replace(fresh)
		metrics.RetainCudaSeries(r.buildActiveCudaKeys(fresh))

		current := readDroppedTotal(droppedMap)
		switch {
		case !droppedBaselined:
			// 첫 호출: baseline 만 저장하고 add 건너뜀.
			lastDroppedTotal = current
			droppedBaselined = true
		case current < lastDroppedTotal:
			// BPF map reset 등 정상적으로는 일어나기 어려운 케이스. 거짓 spike 회피 위해 가산 skip + 새 baseline.
			lastDroppedTotal = current
		case current > lastDroppedTotal:
			metrics.AddCudaEventsLost(r.nodeName, current-lastDroppedTotal)
			lastDroppedTotal = current
		}
		// current == lastDroppedTotal 인 케이스 (drop 변화 없음) 는 모든 분기를 통과하지 않아 자연 no-op.
	}

	// 첫 풀링을 즉시 1회 수행해 reader 가 첫 이벤트를 받기 전 매핑이 채워지도록 한다.
	refresh()

	t := time.NewTicker(r.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

// readDroppedTotal 은 cuda_dropped percpu array (key=0) 슬롯을 모든 CPU 에서 읽어 합산한다.
// percpu 슬롯이라 각 CPU 가 자기 슬롯만 비-원자 증가시키므로 합산 시점의 값이 한 단계 늦을 수
// 있지만, baseline-then-delta 패턴이라 다음 폴에서 자연스럽게 흡수된다. lookup 자체가 실패하면
// 0 을 반환해 delta 가산 없이 무해하게 진행한다.
func readDroppedTotal(droppedMap *cebpf.Map) uint64 {
	var perCPU []uint64
	var key uint32 = 0
	if err := droppedMap.Lookup(key, &perCPU); err != nil {
		log.Printf("cuda dropped map lookup: %v", err)
		return 0
	}
	var sum uint64
	for _, v := range perCPU {
		sum += v
	}
	return sum
}

// buildActiveCudaKeys 는 PID→GPU UUID 스냅샷에서 PodResolver 로 Pod 식별까지 끝낸
// (node, namespace, pod, uid, gpu_uuid) 라벨 키 셋을 만든다. RetainCudaSeries 가 cuda counter
// 의 stale 시리즈 cleanup 기준으로 사용한다. resolver 가 nil 이거나 어떤 PID 도 Pod 으로
// 해석되지 않으면 빈 셋을 반환해 모든 시리즈가 제거된다.
//
// PodName/PodUID 폴백은 metrics.PodNameOrUnknown / PodUIDOrUnknown 을 그대로 사용해
// RecordCudaEvent 가 만드는 라벨 키와 정확히 동일한 형식이 보장된다 (cleanup 매칭 정합성).
func (r *Reader) buildActiveCudaKeys(pidToUUID map[uint32]string) map[metrics.CudaLabelKey]struct{} {
	if r.resolver == nil {
		return map[metrics.CudaLabelKey]struct{}{}
	}
	active := make(map[metrics.CudaLabelKey]struct{}, len(pidToUUID))
	for pid, uuid := range pidToUUID {
		// dispatch hot path 와 동일한 캐시를 공유한다. production refresh cycle 에서 호출된
		// 경우에는 직전에 r.pods.replace(r.resolvePidToPod(fresh)) 가 동일 fresh 셋으로 캐시를
		// 일괄 적재하므로 모든 lookup 이 hit 로 끝나며 fallback 분기는 도달하지 않는다.
		// fallback 은 unit test 등 캐시 사전 적재 없이 본 함수를 직접 호출하는 경로의 함수
		// 계약 (self-contained) 을 유지하기 위한 robustness 가드다 — 이 분기를 제거하려면
		// 모든 호출자가 사전 replace 를 책임지도록 결합도가 올라간다.
		id, ok := r.pods.lookup(pid)
		if !ok {
			id = r.resolver.ResolvePID(pid)
			r.pods.store(pid, id)
		}
		if !id.IsPod() {
			continue
		}
		active[metrics.CudaActiveKey(r.nodeName, id.NamespaceLabel(), metrics.PodNameOrUnknown(id), metrics.PodUIDOrUnknown(id), uuid)] = struct{}{}
	}
	return active
}

// resolvePidToPod 는 NVML refresh 사이클에서 active PID 셋을 받아 ResolvePID 를 PID 당 한
// 번씩만 호출해 fresh PodIdentity 매핑을 만든다. 결과는 podMap.replace 로 통째 적재되어
// dispatch hot path 가 다음 refresh 까지의 모든 이벤트를 캐시 hit 로 처리하게 한다.
//
// resolver 가 nil 인 경우 빈 매핑을 반환해 podMap 도 빈 상태로 통째 교체된다 (dispatch 가
// resolver==nil 분기로 분기되므로 lookup 결과가 사용되지 않는다).
//
// negative result (비-Pod) 도 zero PodIdentity 그대로 적재해 dispatch 가 동일 PID 에 대해
// ResolvePID 를 다시 호출하지 않게 한다 (lazy fill 경로의 negative caching 과 동일 의미).
func (r *Reader) resolvePidToPod(pidToUUID map[uint32]string) map[uint32]kube.PodIdentity {
	if r.resolver == nil {
		return map[uint32]kube.PodIdentity{}
	}
	fresh := make(map[uint32]kube.PodIdentity, len(pidToUUID))
	for pid := range pidToUUID {
		fresh[pid] = r.resolver.ResolvePID(pid)
	}
	return fresh
}

// collectPidToUUID 는 NVML 의 모든 device 에서 RunningProcesses 를 수집해 PID 를 GPU UUID 로
// 매핑한다. 같은 PID 가 여러 GPU 에 동시 등장하면 마지막으로 본 GPU 가 매핑에 남으며 (last-wins),
// 그런 PID 개수 (multiGPUCount) 를 같이 반환해 호출자가 운영 메트릭으로 노출할 수 있게 한다.
// last-wins 동작은 단일 프로세스 multi-GPU 워크로드 (nn.DataParallel, 모델 병렬 등) 에서만 정확도가
// 떨어지며, DDP 류 GPU 당 1 프로세스 패턴은 영향 없다. 정확한 per-event GPU attribution 은 후속
// commit 의 BPF TID→device 추적으로 보강된다 (#33).
func (r *Reader) collectPidToUUID(devices []nvml.Device) (map[uint32]string, int) {
	fresh := make(map[uint32]string)
	pidGPUCount := make(map[uint32]int)
	for _, dev := range devices {
		info, err := dev.Info()
		if err != nil {
			continue
		}
		procs, err := dev.RunningProcesses()
		if err != nil {
			continue
		}
		for _, p := range procs {
			fresh[p.PID] = info.UUID
			pidGPUCount[p.PID]++
		}
	}
	multiGPUCount := 0
	for _, n := range pidGPUCount {
		if n > 1 {
			multiGPUCount++
		}
	}
	return fresh, multiGPUCount
}
