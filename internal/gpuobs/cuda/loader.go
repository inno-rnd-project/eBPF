package cuda

import (
	"bytes"
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

// trackedSymbols 는 attach 시도할 libcuda 심볼 7종이다. 슬라이스 순서는 Reader.Run 의
// attach 루프와 metrics 의 symbol_available 라벨에 그대로 노출된다.
//
// 방향이 인자에서 결정되는 cuMemcpy / cuMemcpy2D* / cuMemcpy3D* / cuMemcpyDtoD* 류는 본 모듈
// 비목표이며 후속 이슈에서 분리해 다룬다 (issue #24 코멘트 참고). 추가 심볼은 BPF 측 SEC 정의와
// 함께 동시에 갱신해야 한다.
var trackedSymbols = []string{
	"cuLaunchKernel",
	"cuLaunchKernelEx",
	"cuLaunchCooperativeKernel",
	"cuMemcpyHtoD_v2",
	"cuMemcpyHtoDAsync_v2",
	"cuMemcpyDtoH_v2",
	"cuMemcpyDtoHAsync_v2",
}

// Reader 는 cuda uprobe 가 emit 한 ringbuf 이벤트의 소비자다. lifecycle 은 Run 이 소유한다.
type Reader struct {
	libcudaPath  string
	nodeName     string
	nv           nvml.NVML
	resolver     PodResolver
	refreshEvery time.Duration

	// recordEvent 는 metrics.RecordCudaEvent 를 위한 test seam 이다.
	// 운영 코드는 New 에서 metrics.RecordCudaEvent 를 기본값으로 받고, 단위 테스트에서는
	// spy 함수로 교체해 dispatch 가 산출한 sample 을 검증한다.
	recordEvent func(node string, sample metrics.CudaEventSample)
}

// New 는 Reader 를 생성한다.
//   - libcudaPath: host 의 libcuda.so.1 절대경로. DaemonSet hostPath 마운트 결과 (예: /host/usr/lib/x86_64-linux-gnu/libcuda.so.1).
//   - nodeName:    metric 라벨 / log 컨텍스트 용 노드명.
//   - nv:          NVML 핸들. nil 이면 device map refresher 가 즉시 종료해 모든 이벤트가 gpu_uuid="unknown" 으로 발행된다.
//   - resolver:    PID→Pod 해상도 제공자. nil 이면 모든 이벤트가 비-Pod 로 분류되어 metrics 측에서 발행 skip 된다.
//   - refreshEvery: device map refresh 주기. 0 이하 값은 호출자가 사전 검증해야 한다.
func New(libcudaPath, nodeName string, nv nvml.NVML, resolver PodResolver, refreshEvery time.Duration) *Reader {
	return &Reader{
		libcudaPath:  libcudaPath,
		nodeName:     nodeName,
		nv:           nv,
		resolver:     resolver,
		refreshEvery: refreshEvery,
		recordEvent:  metrics.RecordCudaEvent,
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

	ex, err := link.OpenExecutable(r.libcudaPath)
	if err != nil {
		return fmt.Errorf("open libcuda %q: %w", r.libcudaPath, err)
	}

	progBySymbol := map[string]*cebpf.Program{
		"cuLaunchKernel":            objs.HandleCuLaunchKernel,
		"cuLaunchKernelEx":          objs.HandleCuLaunchKernelEx,
		"cuLaunchCooperativeKernel": objs.HandleCuLaunchCooperativeKernel,
		"cuMemcpyHtoD_v2":           objs.HandleCuMemcpyHtod,
		"cuMemcpyHtoDAsync_v2":      objs.HandleCuMemcpyHtodAsync,
		"cuMemcpyDtoH_v2":           objs.HandleCuMemcpyDtoh,
		"cuMemcpyDtoHAsync_v2":      objs.HandleCuMemcpyDtohAsync,
	}

	var links []link.Link
	defer func() {
		for _, l := range links {
			_ = l.Close()
		}
	}()

	attached := 0
	for _, sym := range trackedSymbols {
		prog := progBySymbol[sym]
		l, err := ex.Uprobe(sym, prog, nil)
		if err != nil {
			metrics.SetCudaSymbolAvailability(r.nodeName, sym, false)
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
	log.Printf("cuda uprobe attached %d/%d symbols on %s", attached, len(trackedSymbols), r.libcudaPath)

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
		r.runDeviceMapRefresher(runCtx, devmap)
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

		var raw rawEvent
		if err := binary.Read(bytes.NewReader(record.RawSample), binary.NativeEndian, &raw); err != nil {
			log.Printf("decode cuda event: %v", err)
			continue
		}

		r.dispatch(raw, devmap)
	}
}

// dispatch 는 한 raw 이벤트를 PID → PodIdentity / PID → GPU UUID 까지 해상도하고,
// metrics.RecordCudaEvent (또는 test seam recordEvent) 로 누적시킨다.
// 비-Pod 식별자 / 정의되지 않은 kind / 미해상도 GPU UUID 폴백은 모두 metrics 계층에서 흡수된다.
func (r *Reader) dispatch(raw rawEvent, devmap *deviceMap) {
	var id kube.PodIdentity
	if r.resolver != nil {
		id = r.resolver.ResolvePID(raw.PID)
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
// (Pod, GPU) 의 metric 시리즈를 surgical Delete 한다. ctx 종료 시 device handle 을 모두 Close 한다.
// nv 가 nil 이면 즉시 종료한다 — 이 경우 deviceMap 은 비어 있어 모든 이벤트가 gpu_uuid="unknown" 으로 발행되고
// 시리즈 cleanup 도 일어나지 않는다.
func (r *Reader) runDeviceMapRefresher(ctx context.Context, devmap *deviceMap) {
	if r.nv == nil {
		return
	}

	devices := r.discoverDevices()
	defer func() {
		for _, dev := range devices {
			_ = dev.Close()
		}
	}()

	refresh := func() {
		fresh := r.collectPidToUUID(devices)
		devmap.replace(fresh)
		metrics.RetainCudaSeries(r.buildActiveCudaKeys(fresh))
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

// buildActiveCudaKeys 는 PID→GPU UUID 스냅샷에서 PodResolver 로 Pod 식별까지 끝낸
// (node, namespace, pod, uid, gpu_uuid) 라벨 키 셋을 만든다. RetainCudaSeries 가 cuda counter
// 의 stale 시리즈 cleanup 기준으로 사용한다. resolver 가 nil 이거나 어떤 PID 도 Pod 으로
// 해석되지 않으면 빈 셋을 반환해 모든 시리즈가 제거된다.
func (r *Reader) buildActiveCudaKeys(pidToUUID map[uint32]string) map[string]struct{} {
	if r.resolver == nil {
		return map[string]struct{}{}
	}
	active := make(map[string]struct{}, len(pidToUUID))
	for pid, uuid := range pidToUUID {
		id := r.resolver.ResolvePID(pid)
		if !id.IsPod() {
			continue
		}
		active[metrics.CudaActiveKey(r.nodeName, id.NamespaceLabel(), podOrUnknown(id.PodName), podOrUnknown(id.PodUID), uuid)] = struct{}{}
	}
	return active
}

// podOrUnknown 은 metrics 패키지의 podName/podUID fallback 정책과 정확히 일치해야 cleanup 키와
// RecordCudaEvent 키가 매칭된다. 빈 값은 metrics 측에서 "unknown" 라벨로 기록되므로 동일 폴백을 적용한다.
func podOrUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (r *Reader) discoverDevices() []nvml.Device {
	count, err := r.nv.DeviceCount()
	if err != nil {
		log.Printf("cuda: device count: %v", err)
		return nil
	}
	devices := make([]nvml.Device, 0, count)
	for i := uint(0); i < count; i++ {
		dev, err := r.nv.Device(i)
		if err != nil {
			log.Printf("cuda: device idx=%d: %v", i, err)
			continue
		}
		devices = append(devices, dev)
	}
	return devices
}

func (r *Reader) collectPidToUUID(devices []nvml.Device) map[uint32]string {
	fresh := make(map[uint32]string)
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
		}
	}
	return fresh
}
