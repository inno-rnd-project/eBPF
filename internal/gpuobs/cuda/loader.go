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
	"netobs/internal/selfobs"
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
// trackedUretprobes 는 libcuda.so 에 attach 시도할 uretprobe 심볼이다. uprobe 와 다르게 함수 return
// 시점에 fire 하며, 본 PR 은 cuCtxCreate_v2 의 출력 ctx 포인터를 capture 해 ctx-to-device 매핑을 만드는
// 한 가지 용도로만 사용한다 (#33). 동일 심볼이 trackedSymbols 에도 들어 있으면 entry 와 exit 양쪽에
// uprobe / uretprobe 가 함께 attach 되어야 한다.
var trackedUretprobes = []string{
	"cuCtxCreate_v2",
	// #67 의 동기화 latency 측정용. entry uprobe 가 ts_ns 를 stash 하고 본 uretprobe 가 차분해
	// ringbuf 로 emit 한다. entry / exit 어느 한쪽이라도 attach 실패 시 latency 측정이 깨지므로
	// SetCudaSymbolAvailability 가 0 으로 override 된다.
	"cuStreamSynchronize",
	"cuEventSynchronize",
}

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
	// 아래는 #33 의 multi-GPU attribution 용 control-plane uprobe 들이다. 이벤트를 emit 하지 않고
	// BPF map 만 갱신해 dispatch 시점의 GPU ordinal 분리 정확도를 높인다.
	"cuCtxCreate_v2",
	"cuCtxSetCurrent",
	// #67 의 동기화 latency / counter 용 entry uprobe 3 종. cuStreamSynchronize 와 cuEventSynchronize
	// 는 uretprobe 와 함께 entry-exit 페어로 latency 산정, cuStreamWaitEvent 는 host blocking 없는
	// non-blocking call 이라 호출 빈도 counter 로만 노출한다.
	"cuStreamSynchronize",
	"cuEventSynchronize",
	"cuStreamWaitEvent",
}

// cudartTrackedSymbols 는 libcudart.so 에 attach 시도할 CUDA Runtime API 심볼이다.
// libcuda 와 분리된 OpenExecutable 로 처리되며, libcudartPath 가 빈 값이면 본 attach 자체가 skip 된다.
// cudaSetDevice 는 dispatch 의 GPU attribution 정확도를 위해 TID → device 매핑을 BPF map 에
// 기록하는 용도라 이벤트를 emit 하지 않는다 (#33).
var cudartTrackedSymbols = []string{
	"cudaLaunchKernel",
	"cudaMemcpy",
	"cudaMemcpyAsync",
	"cudaSetDevice",
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

	// visDev 는 PID 별 NVIDIA_VISIBLE_DEVICES 해석 결과 (컨테이너 ordinal → 호스트 UUID) 를 캐시한다.
	// dispatch 가 BPF 의 device_ord 를 호스트 NVML UUID 로 변환할 때 lookup 한다 (#33).
	visDev *visDevMap

	// hostUUIDByIndex / hostUUIDSet 은 NVML 이 반환하는 호스트 GPU UUID 를 NVML index 와 함께 보관한
	// 매핑이며, NVIDIA_VISIBLE_DEVICES 의 "all" / index list / UUID list 해석에 사용된다. NVML
	// refresh 사이클에서 갱신된다.
	hostUUIDByIndex map[int]string
	hostUUIDSet     map[string]struct{}
	hostUUIDMu      sync.RWMutex

	// mapSizers 는 #413 utilization 편입 대상 cuda BPF map (cuda_tid_device, cuctx_to_device,
	// cuctx_create_args, sync_starts) 의 sizer 다. Run 이 BPF objs 로드 직후 구성하고 refreshOnce
	// 가 mapUtilEmitEvery 간격으로 iterate 해 gpuobs_bpf_map_utilization_ratio 를 emit 한다.
	// refresh 주기 (기본 1s) 마다 iterate 하면 낭비라 별도 간격으로 묶는다.
	mapSizers   []selfobs.MapSizer
	lastMapUtil time.Time

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
		libcudaPath:     libcudaPath,
		libcudartPath:   libcudartPath,
		nodeName:        nodeName,
		nv:              nv,
		resolver:        resolver,
		refreshEvery:    refreshEvery,
		pods:            newPodMap(),
		visDev:          newVisDevMap(),
		hostUUIDByIndex: map[int]string{},
		hostUUIDSet:     map[string]struct{}{},
		recordEvent:     metrics.RecordCudaEvent,
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
		"cuCtxCreate_v2":            objs.HandleCuCtxCreateV2Entry,
		"cuCtxSetCurrent":           objs.HandleCuCtxSetCurrent,
		"cuStreamSynchronize":       objs.HandleCuStreamSynchronizeEntry,
		"cuEventSynchronize":        objs.HandleCuEventSynchronizeEntry,
		"cuStreamWaitEvent":         objs.HandleCuStreamWaitEventEntry,
	}

	// progByUretprobeSymbol 은 uretprobe 로 attach 할 program 을 묶는다. cuCtxCreate_v2 의 ctx-to-device
	// 매핑 + #67 의 동기화 latency 측정 (cuStreamSynchronize, cuEventSynchronize) 의 exit 페어가 본
	// map 에 등록된다.
	progByUretprobeSymbol := map[string]*cebpf.Program{
		"cuCtxCreate_v2":      objs.HandleCuCtxCreateV2Exit,
		"cuStreamSynchronize": objs.HandleCuStreamSynchronizeExit,
		"cuEventSynchronize":  objs.HandleCuEventSynchronizeExit,
	}

	// libcudart 의 program 매핑은 libcudart 경로가 비어 있어도 항상 노출되며, 실제 attach 는 libcudartPath
	// 가 비어있지 않을 때만 수행된다.
	cudartProgBySymbol := map[string]*cebpf.Program{
		"cudaLaunchKernel": objs.HandleCudaLaunchKernel,
		"cudaMemcpy":       objs.HandleCudaMemcpy,
		"cudaMemcpyAsync":  objs.HandleCudaMemcpyAsync,
		"cudaSetDevice":    objs.HandleCudaSetDevice,
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
		// uprobe_multi link 경로 (kernel 6.6+, BPF_LINK_CREATE with BPF_TRACE_UPROBE_MULTI) 를 사용한다.
		// 기존 link.Uprobe 는 perf_event_open uprobe PMU 를 호출해 kernel.perf_event_paranoid 정책의
		// 차단을 받는데 (Ubuntu 의 paranoid=4 환경에서 EACCES), uprobe_multi link 는 perf 경로 자체를
		// 우회해 CAP_BPF + CAP_PERFMON + CAP_SYS_PTRACE 만으로 attach 가 성립한다. 심볼 단위 호출이라
		// "multi" 라는 이름과 달리 batch 효과는 없지만 attach mechanism 만 교체하는 게 본 변경의 목적이다.
		l, err := ex.UprobeMulti([]string{sym}, prog, nil)
		if err != nil {
			log.Printf("cuda uprobe attach %s: %v", sym, err)
			continue
		}
		links = append(links, l)
		metrics.SetCudaSymbolAvailability(r.nodeName, sym, true)
		attached++
	}
	if attached == 0 {
		return fmt.Errorf("no cuda uprobe attached; check libcuda path %q, kernel >= 6.6 (uprobe_multi link), and CAP_BPF/CAP_PERFMON/CAP_SYS_PTRACE", r.libcudaPath)
	}

	// uretprobe attach 루프. 실패는 warn 로깅 후 진행해 multi-GPU attribution 일부가 비활성이어도
	// 본 PR 의 다른 기능 (kernel launch / memcpy 카운터, podMap 캐시) 이 그대로 작동하게 한다.
	// 단, 본 PR 의 cuCtxCreate_v2 같은 entry+exit 페어 심볼은 둘 중 하나라도 실패하면 ctx-to-device
	// 매핑이 만들어지지 않아 multi-GPU attribution 의 Driver API 경로가 작동하지 않는다.
	// gpuobs_cuda_symbol_available 가 운영자 진단의 1차 신호라 entry 만 attach 된 half-attached
	// 상태를 0 으로 override 해 진단 정확성을 보존한다.
	for _, sym := range trackedUretprobes {
		prog, ok := progByUretprobeSymbol[sym]
		if !ok || prog == nil {
			log.Printf("cuda uretprobe attach %s: missing ebpf program mapping", sym)
			metrics.SetCudaSymbolAvailability(r.nodeName, sym, false)
			continue
		}
		l, err := ex.UretprobeMulti([]string{sym}, prog, nil)
		if err != nil {
			log.Printf("cuda uretprobe attach %s: %v", sym, err)
			metrics.SetCudaSymbolAvailability(r.nodeName, sym, false)
			continue
		}
		links = append(links, l)
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
				l, err := cudartEx.UprobeMulti([]string{sym}, prog, nil)
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

	// #413 cuda LRU map utilization sizer. 구성 실패는 해당 map 만 skip (fail-open).
	for _, cm := range []struct {
		name string
		m    *cebpf.Map
	}{
		{"cuda_tid_device", objs.CudaTidDevice},
		{"cuctx_to_device", objs.CuctxToDevice},
		{"cuctx_create_args", objs.CuctxCreateArgs},
		{"sync_starts", objs.SyncStarts},
	} {
		if sizer, err := selfobs.NewBPFMapSizer(cm.name, cm.m); err == nil {
			r.mapSizers = append(r.mapSizers, sizer)
		} else {
			log.Printf("cuda: %s sizer skipped: %v", cm.name, err)
		}
	}

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
		TsNs:      binary.NativeEndian.Uint64(b[0:8]),
		Bytes:     binary.NativeEndian.Uint64(b[8:16]),
		PID:       binary.NativeEndian.Uint32(b[16:20]),
		TID:       binary.NativeEndian.Uint32(b[20:24]),
		Kind:      b[24],
		DeviceOrd: binary.NativeEndian.Uint32(b[28:32]),
		LatencyNs: binary.NativeEndian.Uint64(b[32:40]),
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
		ID:        id,
		GPUUUID:   r.resolveGPUUUID(raw, devmap),
		Kind:      types.CudaEventKind(raw.Kind),
		Bytes:     raw.Bytes,
		LatencyNs: raw.LatencyNs,
	})
}

// resolveGPUUUID 는 BPF 가 capture 한 device_ord 와 visDev 캐시를 우선 사용해 GPU UUID 를 분리하고,
// ordinal 매핑이 없을 때 (BPF map miss / NVIDIA_VISIBLE_DEVICES 비어 있음 / unknown index) 기존
// PID-level devmap.lookup 으로 폴백한다. 본 함수가 dispatch hot path 의 GPU attribution 분리 결정
// 지점이며, multi-GPU PID 의 per-event 정확도를 visDev hit 로 끌어올리고 single-GPU PID 의 회귀를
// devmap fallback 으로 보존한다 (#33).
//
// hot path 비용 최소화를 위해 visDev 는 단일 lookup 만 거친다. UNKNOWN sentinel 은 분기 직전에
// 거른다.
func (r *Reader) resolveGPUUUID(raw rawEvent, devmap *deviceMap) string {
	if raw.DeviceOrd == CudaDeviceOrdUnknown {
		return devmap.lookup(raw.PID)
	}
	ords, ok := r.visDev.lookup(raw.PID)
	if !ok {
		// visDev miss: NVIDIA_VISIBLE_DEVICES 매핑이 적재되지 않은 신규 PID 의 경우 lazy fill 로
		// /proc/<pid>/environ 을 한 번 읽어 캐시한 뒤 같은 ordinal 을 재시도한다. 두 번째 이벤트부터
		// hit 경로로 들어간다. 같은 PID 의 lazy fill 이 동시 다발해도 podMap.store 와 동일하게
		// Lock 으로 직렬화되어 정합성 문제 없다.
		ords = r.lazyFillVisDev(raw.PID)
	}
	idx := int(raw.DeviceOrd)
	if idx >= 0 && idx < len(ords) && ords[idx] != "" {
		return ords[idx]
	}
	return devmap.lookup(raw.PID)
}

// lazyFillVisDev 는 visDev 캐시 miss 시 /proc/<pid>/environ 을 읽어 해석 결과를 적재하고
// 적재한 슬라이스를 그대로 반환한다. environ read 실패는 nil 슬라이스로 negative cache 에
// 적재해 동일 PID 의 후속 이벤트가 lazy fill 을 다시 시도하지 않게 한다.
func (r *Reader) lazyFillVisDev(pid uint32) []string {
	r.hostUUIDMu.RLock()
	hostByIdx := r.hostUUIDByIndex
	hostSet := r.hostUUIDSet
	r.hostUUIDMu.RUnlock()
	value, err := readNVIDIAVisibleDevices(pid)
	if err != nil {
		r.visDev.store(pid, nil)
		return nil
	}
	ords := parseVisibleDevices(value, hostByIdx, hostSet)
	r.visDev.store(pid, ords)
	return ords
}

// droppedSource 는 BPF cuda_dropped percpu map 의 누적값을 추상화해 통합 테스트가 BPF kernel
// 호출 없이 fake 카운터를 주입할 수 있게 한다. production 경로는 bpfDroppedSource 가 *cebpf.Map
// 을 감싸 동일한 의미를 보존한다.
type droppedSource interface {
	Total() uint64
}

// bpfDroppedSource 는 production 의 cuda_dropped percpu map 어댑터다.
type bpfDroppedSource struct {
	m *cebpf.Map
}

func (b bpfDroppedSource) Total() uint64 { return readDroppedTotal(b.m) }

// droppedBaseline 은 metrics ECC / Violation / Energy / PcieReplay 와 동일한 baseline-then-delta
// 추적기다. 첫 호출은 baseline 만 저장하고 add 를 건너뛰며, current < last 인 reset 케이스는
// 거짓 spike 회피를 위해 가산 skip + 새 baseline 으로 갱신한다. 통합 테스트가 사이클 단위 호출을
// 하기 위해 closure 변수를 외부 struct 로 격상시켰다.
type droppedBaseline struct {
	last        uint64
	initialized bool
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

	dropped := bpfDroppedSource{m: droppedMap}
	var baseline droppedBaseline

	// 첫 풀링을 즉시 1회 수행해 reader 가 첫 이벤트를 받기 전 매핑이 채워지도록 한다.
	r.refreshOnce(devSet, devmap, dropped, &baseline)

	t := time.NewTicker(r.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refreshOnce(devSet, devmap, dropped, &baseline)
		}
	}
}

// refreshOnce 는 한 ticker 사이클의 모든 작업을 수행한다. NVML device 동기화, PID→UUID 수집,
// podMap / visDev / devmap 통째 교체, RetainCudaSeries cleanup, dropped counter delta 발행이
// 단일 호출 안에서 끝난다. runDeviceMapRefresher 가 ticker 와 lifecycle 만 담당하고 본 함수가
// 비즈니스 로직 전체를 책임지는 구조라 통합 테스트가 본 함수를 직접 호출해 사이클 단위 정합성을
// 검증할 수 있다.
//
// 첫 호출 시점에 BPF percpu map 이 0 이 아닐 가능성 (cuda reader 가 attach 보다 먼저 map 을
// 만들고 다른 프로세스가 그 사이 reserve 실패한 케이스 / agent 재시작 시 map 잔존 등) 이 있어
// baseline.initialized=false 인 첫 호출은 last 만 저장하고 delta 가산을 건너뛴다.
func (r *Reader) refreshOnce(devSet *nvml.DeviceSet, devmap *deviceMap, dropped droppedSource, baseline *droppedBaseline) {
	// 매 cycle 에 device 셋을 동기화해 hot-plug 변화를 흡수한다. Sync 실패는 warn 로그만 남기고
	// 직전까지의 device 슬라이스를 그대로 사용한다 (다음 cycle 에서 재시도).
	if err := devSet.Sync(); err != nil {
		log.Printf("cuda: device sync: %v", err)
	}
	fresh, freshAll, multiGPUCount := r.collectPidToUUID(devSet.Snapshot())
	metrics.SetCudaPidMultiGPUCount(r.nodeName, multiGPUCount)
	// active PID 셋에 대해 ResolvePID 를 본 사이클에서 한 번씩만 호출해 podMap 을 통째 교체한다.
	// dispatch hot path 는 다음 사이클까지의 모든 이벤트를 캐시 hit 로 처리하고, 종료된 PID 는
	// 본 replace 로 자연스럽게 제거되어 별도 cleanup 호출이 필요하지 않다.
	r.pods.replace(r.resolvePidToPod(fresh))
	// active PID 의 NVIDIA_VISIBLE_DEVICES 를 일괄 해석해 visDev 캐시도 통째 교체한다.
	// dispatch 의 ordinal-to-UUID 변환이 hit 경로로 들어가게 한다 (#33).
	r.visDev.replace(r.resolveVisibleDevices(fresh))
	devmap.replace(fresh)
	metrics.RetainCudaSeries(r.buildActiveCudaKeys(freshAll))

	current := dropped.Total()
	switch {
	case !baseline.initialized:
		baseline.last = current
		baseline.initialized = true
	case current < baseline.last:
		// BPF map reset 등 정상적으로는 일어나기 어려운 케이스. 거짓 spike 회피 위해 가산 skip + 새 baseline.
		baseline.last = current
	case current > baseline.last:
		metrics.AddCudaEventsLost(r.nodeName, current-baseline.last)
		baseline.last = current
	}
	// current == baseline.last 인 케이스 (drop 변화 없음) 는 모든 분기를 통과하지 않아 자연 no-op.

	r.emitMapUtilization()
}

// mapUtilEmitEvery 는 cuda BPF map utilization iterate 의 최소 간격이다. refresh 사이클 (기본 1s)
// 마다 수만 entry iterate 는 낭비라 netobs self-health refresher 와 동일한 30s cadence 로 묶는다.
const mapUtilEmitEvery = 30 * time.Second

// emitMapUtilization 은 mapUtilEmitEvery 이상 지난 경우에만 sizer 4종을 iterate 해
// gpuobs_bpf_map_utilization_ratio 를 emit 한다. refreshOnce 단일 goroutine 에서만 호출되어
// lastMapUtil 에 동기화가 필요 없다.
func (r *Reader) emitMapUtilization() {
	if len(r.mapSizers) == 0 || time.Since(r.lastMapUtil) < mapUtilEmitEvery {
		return
	}
	r.lastMapUtil = time.Now()
	for _, s := range r.mapSizers {
		entries, err := s.Entries()
		if err != nil {
			log.Printf("cuda: map %s entries iterate: %v", s.Name(), err)
			continue
		}
		max := s.MaxEntries()
		if max == 0 {
			continue
		}
		metrics.SetBpfMapUtilization(s.Name(), float64(entries)/float64(max))
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
// pidToUUIDs 는 PID 당 본 모든 GPU UUID 의 리스트라 multi-GPU PID 의 모든 (PID, GPU) 시리즈가
// active 셋에 포함된다. dispatch 가 device_ord + visDev 로 정확한 GPU 라벨로 발행한 시리즈가
// cleanup 사이클에서 부당하게 제거되지 않게 한다 (#33). 단일 GPU PID 는 길이 1 슬라이스로 들어와
// 기존 동작과 동일하게 한 키만 만든다.
//
// PodName/PodUID 폴백은 metrics.PodNameOrUnknown / PodUIDOrUnknown 을 그대로 사용해
// RecordCudaEvent 가 만드는 라벨 키와 정확히 동일한 형식이 보장된다 (cleanup 매칭 정합성).
func (r *Reader) buildActiveCudaKeys(pidToUUIDs map[uint32][]string) map[metrics.CudaLabelKey]struct{} {
	if r.resolver == nil {
		return map[metrics.CudaLabelKey]struct{}{}
	}
	active := make(map[metrics.CudaLabelKey]struct{}, len(pidToUUIDs))
	for pid, uuids := range pidToUUIDs {
		// dispatch hot path 와 동일한 캐시를 공유한다. production refresh cycle 에서 호출된
		// 경우에는 직전에 r.pods.replace(r.resolvePidToPod(fresh)) 가 동일 fresh 셋으로 캐시를
		// 일괄 적재하므로 모든 lookup 이 hit 로 끝나며 fallback 분기는 도달하지 않는다.
		// fallback 은 unit test 등 캐시 사전 적재 없이 본 함수를 직접 호출하는 경로의 함수
		// 계약 (self-contained) 을 유지하기 위한 robustness 가드다.
		id, ok := r.pods.lookup(pid)
		if !ok {
			id = r.resolver.ResolvePID(pid)
			r.pods.store(pid, id)
		}
		if !id.IsPod() {
			continue
		}
		for _, uuid := range uuids {
			active[metrics.CudaActiveKey(r.nodeName, id.NamespaceLabel(), metrics.PodNameOrUnknown(id), metrics.PodUIDOrUnknown(id), uuid)] = struct{}{}
		}
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
// #413 부터 직전 사이클의 podMap 결과를 carry-over 한다. 살아 있는 PID 의 /proc/<pid>/cgroup
// 경로는 프로세스 수명 동안 불변이라 Pod 해석 결과 (positive) 는 재파싱 없이 재사용해도 안전
// 하고, 기본 1s 주기에서 전 활성 PID 의 procfs 재파싱이 신규 PID 한정으로 준다. negative result
// (비-Pod / informer 미동기) 는 매 사이클 재해석해 Pod 기동 직후 informer sync 지연이 다음
// 사이클에서 곧바로 수렴하는 종전 동작을 유지하며, dispatch 의 negative caching (사이클 내
// 재호출 억제) 도 그대로다. 사이클 사이의 PID 재사용 (종료 후 같은 PID 로 다른 Pod 프로세스
// 기동) 은 오귀속 여지가 있으나 pid_max 공간에서 1s 창 내 재사용은 실질 확률이 없다.
func (r *Reader) resolvePidToPod(pidToUUID map[uint32]string) map[uint32]kube.PodIdentity {
	if r.resolver == nil {
		return map[uint32]kube.PodIdentity{}
	}
	fresh := make(map[uint32]kube.PodIdentity, len(pidToUUID))
	for pid := range pidToUUID {
		if id, ok := r.pods.lookup(pid); ok && id.IsPod() {
			fresh[pid] = id
			continue
		}
		fresh[pid] = r.resolver.ResolvePID(pid)
	}
	return fresh
}

// collectPidToUUID 는 NVML 의 모든 device 에서 RunningProcesses 를 수집해 PID 를 GPU UUID 로
// 매핑한다. 세 가지 결과를 반환한다.
//
//   - pidToUUID: PID 당 마지막으로 본 GPU UUID (last-wins). devmap 폴백 라벨로 사용된다.
//   - pidToUUIDs: PID 당 본 모든 GPU UUID 의 정렬된 리스트. RetainCudaSeries 가 multi-GPU PID
//     의 모든 (PID, GPU) 시리즈를 active key 셋으로 보존하도록 buildActiveCudaKeys 가 사용한다.
//   - multiGPUCount: 둘 이상 GPU 에 등장한 PID 수. SetCudaPidMultiGPUCount 발행에 사용된다.
//
// last-wins 동작은 BPF 의 cuda_tid_device 매핑이 없을 때 dispatch 가 폴백하는 PID-level 라벨이며,
// 본 PR 의 multi-GPU attribution 은 dispatch 시점에 BPF 가 capture 한 device_ord + visDev resolver
// 로 우선 분리되고 ordinal lookup 이 실패할 때만 본 fresh map 에 폴백한다 (#33).
//
// 본 함수는 collect 도중 hostUUIDByIndex / hostUUIDSet 도 함께 갱신해 visDev 의 NVIDIA_VISIBLE_DEVICES
// 파싱이 최신 NVML index 매핑을 사용하게 한다.
func (r *Reader) collectPidToUUID(devices []nvml.Device) (map[uint32]string, map[uint32][]string, int) {
	fresh := make(map[uint32]string)
	freshAll := make(map[uint32][]string)
	hostByIdx := make(map[int]string, len(devices))
	hostSet := make(map[string]struct{}, len(devices))
	for _, dev := range devices {
		info, err := dev.Info()
		if err != nil {
			continue
		}
		hostByIdx[int(info.Index)] = info.UUID
		hostSet[info.UUID] = struct{}{}
		procs, err := dev.RunningProcesses()
		if err != nil {
			continue
		}
		for _, p := range procs {
			fresh[p.PID] = info.UUID
			freshAll[p.PID] = append(freshAll[p.PID], info.UUID)
		}
	}
	multiGPUCount := 0
	for _, uuids := range freshAll {
		if len(uuids) > 1 {
			multiGPUCount++
		}
	}
	r.hostUUIDMu.Lock()
	r.hostUUIDByIndex = hostByIdx
	r.hostUUIDSet = hostSet
	r.hostUUIDMu.Unlock()
	return fresh, freshAll, multiGPUCount
}

// resolveVisibleDevices 는 active PID 셋에 대해 /proc/<pid>/environ 에서 NVIDIA_VISIBLE_DEVICES 를
// 읽고 호스트 UUID 슬라이스로 해석해 fresh 맵을 만든다. NVML refresh 사이클이 visDev.replace 로
// 통째 교체할 때 사용된다. environ read 실패 (PID 종료 등) 는 nil 슬라이스로 negative cache 에
// 적재해 dispatch 가 동일 PID 에 대해 다시 read 를 시도하지 않게 한다.
func (r *Reader) resolveVisibleDevices(pidToUUID map[uint32]string) map[uint32][]string {
	r.hostUUIDMu.RLock()
	hostByIdx := r.hostUUIDByIndex
	hostSet := r.hostUUIDSet
	r.hostUUIDMu.RUnlock()

	fresh := make(map[uint32][]string, len(pidToUUID))
	for pid := range pidToUUID {
		raw, err := readNVIDIAVisibleDevices(pid)
		if err != nil {
			fresh[pid] = nil
			continue
		}
		fresh[pid] = parseVisibleDevices(raw, hostByIdx, hostSet)
	}
	return fresh
}
