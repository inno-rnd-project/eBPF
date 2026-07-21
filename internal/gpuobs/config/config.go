// Package config는 gpuobs 에이전트의 실행 시 설정을 정의하며, env 값이 기본값이 되고
// CLI flag가 지정되면 env를 덮어쓰는 순서를 따른다. env 형식 오류는 warn 로그를 남기고
// 기본값으로 폴백해 flag가 여전히 최종 값을 덮어쓸 수 있게 한다.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config는 gpuobs 에이전트의 실행 시 설정을 담는다.
type Config struct {
	ListenAddr        string
	NodeName          string
	GPUPollInterval   time.Duration
	GPUMetricsEnabled bool
	// PodMetricsEnabled는 per-pod gauge(`gpuobs_pod_*`) 발행 여부를 결정한다.
	// 대규모 클러스터에서 src_pod / src_pod_uid 라벨 카디널리티 폭증을 막기 위한 escape hatch이며,
	// startup 시점에만 metrics 패키지로 전달되고 그 이후에는 읽기 전용으로 쓴다.
	PodMetricsEnabled bool
	// ContentionEnabled 는 #198 의 pod cgroup 경합 (PSI cpu.pressure / memory.pressure) 수집 여부다.
	// per-pod 경로 (resolver + PodMetricsEnabled) 가 활성일 때만 동작하는 추가 opt-in 이며 기본값 true.
	// GPU Pod 는 소수라 카디널리티가 낮으나, PSI 파일 read 비용을 완전히 끄는 kill-switch 로 둔다.
	ContentionEnabled bool
	// CgroupRoot 는 host cgroup2 계층을 read-only bind mount 한 컨테이너 내 경로다. contention 수집이
	// /proc/<pid>/cgroup 의 경로를 이 루트 아래에서 resolve 한다. 기본값 /host/sys/fs/cgroup.
	CgroupRoot string
	// MetadataRefresh는 kube.Resolver의 informer resync 주기다.
	// 0 이하 값은 의미 없으므로 검증에서 거부된다. netobs와 동일하게 기본값 30s를 쓴다.
	MetadataRefresh time.Duration

	// CudaUprobeEnabled 는 cuda uprobe 모듈 (libcuda.so 심볼 attach + ringbuf reader) 의 활성 여부다.
	// 기본값은 true. 운영 환경에서 PodMetricsEnabled=false 로 카디널리티를 막는 경우에는 본 토글도 함께 false 로 두는 것을 권장한다 (README 참고).
	CudaUprobeEnabled bool
	// CudaUprobeLibcudaPath 는 host 의 libcuda.so.1 절대경로다 (DaemonSet hostPath 마운트 후
	// 컨테이너에서 보이는 경로). 빈 값 (기본) 이면 LibcudaCandidates 를 순회해 첫 실존 경로에
	// attach 한다 (#296, 배포판별 경로 차이 흡수). GPUOBS_CUDA_LIBCUDA_PATH env 로 고정할 수 있다.
	CudaUprobeLibcudaPath string
	// CudaUprobeDeviceMapRefresh 는 cuda 패키지가 NVML RunningProcesses 로 PID→GPU 매핑을 재구축하는 주기다.
	// 0 이하 값은 검증에서 거부된다. 매 사이클마다 RetainCudaSeries 도 함께 수행된다.
	CudaUprobeDeviceMapRefresh time.Duration
	// CudaUprobeLibcudartPath 는 host 의 libcudart.so 절대경로다. 빈 문자열이면 cudart 모듈을 활성화하지 않는다 (default).
	// libcudart 는 NVIDIA driver 가 아닌 CUDA Toolkit 의 일부라 host 에 설치된 환경에서만 의미가 있고,
	// 컨테이너가 자체 libcudart 를 번들링하는 환경에서는 host attach 가 fire 되지 않는다 (README 한계 note 참고).
	CudaUprobeLibcudartPath string

	// CudaLaunchBaselinePerSec는 워크로드의 기대 CUDA kernel launch rate (Hz) 다. correlation 진단의
	// pod:host_compute_stall_score:5m recording rule이 본 메트릭 값을 분모로 써서 launch rate가
	// baseline 이하로 떨어진 비율을 0-1 score로 정규화한다. default 10 hz는 inference workload
	// 기준이며 batch training 등은 더 낮을 수 있어 운영자가 env / flag로 override 한다.
	CudaLaunchBaselinePerSec float64

	// PodUtilAllowNamespaces 는 #104 의 gpuobs_pod_utilization_percent 메트릭이 emit 되는 src namespace
	// 화이트리스트다. 빈 슬라이스가 기본값이며 그때는 전체 클러스터 발행, 명시 시 해당 namespace 의 Pod
	// 만 emit. 본 통제는 신규 util 메트릭에만 적용 되며 기존 PodMetricsEnabled flag 와 별개로 동작하므로
	// pod-metrics 가 활성 이고 본 allow-list 만 일부 namespace 로 좁혀 카디널리티 폭증 방어 가능 하다.
	// netobs 의 NETOBS_FLOW_ALLOW_NAMESPACES 와 동일 parseNamespaceList 패턴 재사용.
	PodUtilAllowNamespaces []string

	// DcgmEnabled는 #123의 NVIDIA DCGM 통합 opt-in 토글이다. 기본값 false로 dev cluster의
	// RTX 3090 환경에서 noopSource만 wire-up되어 gpuobs_dcgm_available이 0 emit된다. 데이터
	// 센터 GPU (A100, H100 등) 환경에서 true로 두면 #133의 dcgm-exporter HTTP Source가
	// 활성되어 DcgmExporterURL의 /metrics를 fetch한다.
	DcgmEnabled bool

	// DcgmExporterURL은 #133의 dcgm-exporter /metrics endpoint URL이다. DcgmEnabled=true일 때
	// dcgm.NewHTTPSource가 본 URL을 fetch해 DCGM hardware counter (PCIe replay count 등) 의
	// reachability를 판정하고 gpuobs_dcgm_available gauge를 set한다. 기본값은 NVIDIA GPU
	// Operator의 표준 dcgm-exporter Service 경로다. dcgm-exporter가 다른 namespace나 port에
	// 배포되면 GPUOBS_DCGM_EXPORTER_URL env로 override한다.
	DcgmExporterURL string

	// NcclEnabled는 #123/#134의 NCCL collective profiler 통합 opt-in 토글이다. 기본값 false로
	// RTX 3090 환경에서 noopProfiler만 wire-up되어 gpuobs_nccl_profiler_available이 0 emit된다.
	// 데이터센터 GPU 환경에서 build tag nccl로 빌드한 이미지에 true로 두면 nccl.NewProduction이
	// NcclLibPath의 libnccl.so.2 collective 심볼에 uprobe를 attach한다. build tag nccl이 비활성인
	// 기본 이미지에서는 true여도 stub이 noop을 돌려줘 graceful degradation을 유지한다.
	NcclEnabled bool

	// NcclLibPath는 #134의 host libnccl.so.2 절대경로다. NcclEnabled=true이고 build tag nccl로
	// 빌드한 이미지일 때 nccl.NewProduction이 본 경로를 OpenExecutable해 collective 심볼에 uprobe를
	// attach한다. 빈 값 (기본) 이면 NcclLibCandidates 순회로 첫 실존 경로를 쓴다 (#296).
	// GPUOBS_NCCL_LIB_PATH env로 고정할 수 있다.
	NcclLibPath string
}

// Parse는 env와 CLI flag를 읽어 Config를 구성해 반환한다.
// env 값이 기본값이 되고 CLI flag가 지정되면 env를 덮어쓴다. env가 형식 오류(예:
// `GPU_POLL_INTERVAL`의 duration 파싱 실패)인 경우 warn 로그를 남기고 기본값으로 폴백해
// -poll-interval 등 flag가 명시되면 그 값이 최종적으로 이기도록 한다. ConfigMap/DaemonSet
// 오타는 warn 로그로 계속 드러나 완전히 숨겨지지 않는다.
// NodeName이 비어 있으면 os.Hostname 결과로 채워진다.
func Parse() (Config, error) {
	pollInterval, err := getenvDuration("GPU_POLL_INTERVAL", 5*time.Second)
	if err != nil {
		// env 형식 오류는 "env < flag 우선순위" 를 끊지 않도록 warn 후 기본값으로 폴백한다.
		// 이후 -poll-interval flag가 명시되면 그 값이 최종적으로 덮어쓴다.
		log.Printf("warn: %v; using default %v", err, pollInterval)
	}

	metadataRefresh, err := getenvDuration("KUBE_METADATA_REFRESH", 30*time.Second)
	if err != nil {
		log.Printf("warn: %v; using default %v", err, metadataRefresh)
	}

	cudaDeviceMapRefresh, err := getenvDuration("GPUOBS_CUDA_DEVICEMAP_REFRESH", 1*time.Second)
	if err != nil {
		log.Printf("warn: %v; using default %v", err, cudaDeviceMapRefresh)
	}

	cudaLaunchBaseline, err := getenvFloat("CUDA_LAUNCH_BASELINE_PER_SEC", 10.0)
	if err != nil {
		log.Printf("warn: %v; using default %v", err, cudaLaunchBaseline)
	}

	cfg := Config{
		ListenAddr:                 getenvDefault("LISTEN_ADDR", ":9820"),
		NodeName:                   getenvDefault("NODE_NAME", ""),
		GPUPollInterval:            pollInterval,
		GPUMetricsEnabled:          getenvBool("GPU_METRICS_ENABLED", true),
		PodMetricsEnabled:          getenvBool("GPUOBS_POD_METRICS_ENABLED", true),
		ContentionEnabled:          getenvBool("GPUOBS_CONTENTION_ENABLED", true),
		CgroupRoot:                 getenvDefault("GPUOBS_CGROUP_ROOT", "/host/sys/fs/cgroup"),
		MetadataRefresh:            metadataRefresh,
		CudaUprobeEnabled:          getenvBool("GPUOBS_CUDA_UPROBE_ENABLED", true),
		CudaUprobeLibcudaPath:      getenvDefault("GPUOBS_CUDA_LIBCUDA_PATH", ""),
		CudaUprobeLibcudartPath:    getenvDefault("GPUOBS_CUDA_LIBCUDART_PATH", ""),
		CudaUprobeDeviceMapRefresh: cudaDeviceMapRefresh,
		CudaLaunchBaselinePerSec:   cudaLaunchBaseline,
		PodUtilAllowNamespaces:     parseNamespaceList(getenvDefault("GPUOBS_POD_UTIL_ALLOW_NAMESPACES", "")),
		DcgmEnabled:                getenvBool("GPUOBS_DCGM_ENABLED", false),
		DcgmExporterURL:            getenvDefault("GPUOBS_DCGM_EXPORTER_URL", "http://dcgm-exporter.gpu-operator.svc:9400/metrics"),
		NcclEnabled:                getenvBool("GPUOBS_NCCL_ENABLED", false),
		NcclLibPath:                getenvDefault("GPUOBS_NCCL_LIB_PATH", ""),
	}

	fs := flag.NewFlagSet("gpuobs-agent", flag.ContinueOnError)
	fs.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "HTTP listen address for metrics and health endpoints")
	fs.StringVar(&cfg.NodeName, "node-name", cfg.NodeName, "observed Kubernetes node name (defaults to hostname when empty)")
	fs.DurationVar(&cfg.GPUPollInterval, "poll-interval", cfg.GPUPollInterval, "NVML polling interval for device snapshots")
	fs.BoolVar(&cfg.GPUMetricsEnabled, "gpu-metrics", cfg.GPUMetricsEnabled, "emit per-device gpuobs_device_* metrics; disable to suppress device-level collection")
	fs.BoolVar(&cfg.PodMetricsEnabled, "pod-metrics", cfg.PodMetricsEnabled, "emit per-pod gpuobs_pod_* metrics; disable on large clusters to cap Prometheus cardinality")
	fs.DurationVar(&cfg.MetadataRefresh, "metadata-refresh", cfg.MetadataRefresh, "Kubernetes metadata informer resync interval")
	fs.BoolVar(&cfg.CudaUprobeEnabled, "cuda-uprobe", cfg.CudaUprobeEnabled, "enable libcuda.so uprobe module emitting gpuobs_cuda_* counters; requires CAP_BPF/CAP_PERFMON/CAP_SYS_PTRACE and a libcuda hostPath mount")
	fs.StringVar(&cfg.CudaUprobeLibcudaPath, "cuda-libcuda-path", cfg.CudaUprobeLibcudaPath, "absolute path to host libcuda.so.1 reachable from inside the container; empty (default) probes the distro candidate paths")
	fs.StringVar(&cfg.CudaUprobeLibcudartPath, "cuda-libcudart-path", cfg.CudaUprobeLibcudartPath, "absolute path to host libcudart.so reachable from inside the container; empty disables cudart attach")
	fs.DurationVar(&cfg.CudaUprobeDeviceMapRefresh, "cuda-devicemap-refresh", cfg.CudaUprobeDeviceMapRefresh, "interval between NVML RunningProcesses sweeps that rebuild the PID→GPU map and clean up stale cuda series")
	fs.Float64Var(&cfg.CudaLaunchBaselinePerSec, "cuda-launch-baseline", cfg.CudaLaunchBaselinePerSec, "expected CUDA kernel launch rate (Hz) used as denominator of pod:host_compute_stall_score:5m correlation rule (default 10)")
	fs.BoolVar(&cfg.DcgmEnabled, "dcgm", cfg.DcgmEnabled, "#123/#133: opt-in NVIDIA DCGM integration; default false keeps the noop source on RTX 3090 so gpuobs_dcgm_available emits 0. When true, the dcgm-exporter HTTP source fetches -dcgm-exporter-url")
	fs.StringVar(&cfg.DcgmExporterURL, "dcgm-exporter-url", cfg.DcgmExporterURL, "#133: dcgm-exporter /metrics endpoint URL fetched by the production DCGM source when -dcgm is enabled")
	fs.BoolVar(&cfg.NcclEnabled, "nccl-profiler", cfg.NcclEnabled, "#123/#134: opt-in NCCL collective profiler; default false keeps the noop profiler on RTX 3090 so gpuobs_nccl_profiler_available emits 0. When true on an image built with the nccl build tag, the production profiler uprobe-attaches the libnccl.so collective symbols at -nccl-lib-path")
	fs.StringVar(&cfg.NcclLibPath, "nccl-lib-path", cfg.NcclLibPath, "#134: absolute path to host libnccl.so.2 reachable from inside the container, uprobe-attached by the production NCCL profiler when -nccl-profiler is enabled on an nccl-tagged image; empty (default) probes the distro candidate paths")
	// -pod-util-allow-namespaces 의 default 를 "unset" sentinel 로 둬, 빈 문자열 명시 (`-pod-util-allow-namespaces=`)
	// 로도 env 값을 덮어 "전체 namespace 발행" 으로 되돌릴 수 있게 한다. 단순 default="" 패턴 으로는 빈 값과
	// 미지정 을 구분 못 해 env override 가 불가능 하다.
	const podUtilAllowUnset = "\x00unset"
	podUtilAllow := podUtilAllowUnset
	fs.StringVar(&podUtilAllow, "pod-util-allow-namespaces", podUtilAllowUnset, "comma-separated namespace allow-list for gpuobs_pod_utilization_percent emission; empty value (e.g. -pod-util-allow-namespaces=) explicitly resets to all-namespaces, unset preserves env. Independent of -pod-metrics gate (which controls pod_memory series)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// -h/-help 요청은 flag 패키지가 usage를 출력한 뒤 ErrHelp를 반환한다.
		// 사용자 의도된 정상 경로이므로 exit 0으로 종료한다.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return Config{}, err
	}

	// -pod-util-allow-namespaces flag 가 명시 (빈 값 포함) 되면 env 값을 덮어쓴다. env < flag 우선순위
	// 약속 유지. sentinel default 와 비교 해 미지정 / 빈 값 / 명시 값 3 분기를 구분 한다.
	if podUtilAllow != podUtilAllowUnset {
		cfg.PodUtilAllowNamespaces = parseNamespaceList(podUtilAllow)
	}

	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return Config{}, fmt.Errorf("listen address must not be empty")
	}

	if cfg.GPUPollInterval <= 0 {
		return Config{}, fmt.Errorf("invalid -poll-interval: must be > 0")
	}

	if cfg.MetadataRefresh <= 0 {
		return Config{}, fmt.Errorf("invalid -metadata-refresh: must be > 0")
	}

	if cfg.CudaLaunchBaselinePerSec <= 0 {
		return Config{}, fmt.Errorf("invalid -cuda-launch-baseline: must be > 0")
	}

	if cfg.CudaUprobeEnabled {
		if cfg.CudaUprobeDeviceMapRefresh <= 0 {
			return Config{}, fmt.Errorf("invalid -cuda-devicemap-refresh: must be > 0")
		}
	}

	if strings.TrimSpace(cfg.NodeName) == "" {
		host, err := os.Hostname()
		if err != nil {
			return Config{}, fmt.Errorf("node name empty and hostname unavailable: %w", err)
		}
		cfg.NodeName = host
	}

	return cfg, nil
}

func getenvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "y"
}

// getenvFloat은 key env를 float64로 파싱해 반환한다. 형식 오류일 때는 호출자가 "env < flag
// 우선순위" 약속을 유지할 수 있도록 기본값과 함께 에러를 돌려주어 호출자가 warn 로그 후 폴백을
// 선택할 수 있게 한다. correlation tunable (CUDA launch baseline 등) 의 env 진입점이다.
func getenvFloat(key string, def float64) (float64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def, fmt.Errorf("invalid float for %s: %q", key, v)
	}
	return f, nil
}

// parseNamespaceList 는 #104 의 GPUOBS_POD_UTIL_ALLOW_NAMESPACES env 와 동명 flag 입력을 namespace
// 슬라이스로 파싱한다. 콤마 구분, 공백 트림, 중복 제거. 빈 입력은 nil 반환 (전체 클러스터 발행 의미).
// netobs 의 parseNamespaceList 와 동일 시맨틱이며 후속 신규 카디널리티 통제 옵션에도 재사용 가능 하다.
func parseNamespaceList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	tokens := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		ns := strings.TrimSpace(tok)
		if ns == "" {
			continue
		}
		if _, ok := seen[ns]; ok {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// getenvDuration은 key env를 duration으로 파싱해 반환한다.
// 형식 오류일 때는 호출자가 "env < flag 우선순위" 약속을 유지할 수 있도록 기본값과
// 함께 에러를 돌려주어, 호출자가 warn 로그 후 폴백을 선택할 수 있게 한다.
func getenvDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("invalid duration for %s: %q", key, v)
	}
	return d, nil
}

// libCandidates 는 배포판과 GPU Operator 구성별 host 라이브러리 후보 경로다 (#296). DaemonSet 이
// host /usr 를 /host/usr 로, /run/nvidia 를 /host/run/nvidia 로 마운트하는 전제와 짝이다. 순서는
// Debian 계열 multiarch, RHEL 계열 lib64, GPU Operator driver 컨테이너 주입 경로 순이다.
func libCandidates(name string) []string {
	return []string{
		"/host/usr/lib/x86_64-linux-gnu/" + name,
		"/host/usr/lib64/" + name,
		// 일부 패키징 (RHEL 계열 RPM, Debian 계열 변형) 은 nvidia 하위 디렉터리에 둔다.
		"/host/usr/lib64/nvidia/" + name,
		"/host/usr/lib/nvidia/" + name,
		"/host/run/nvidia/driver/usr/lib/x86_64-linux-gnu/" + name,
		"/host/run/nvidia/driver/usr/lib64/" + name,
	}
}

// LibcudaCandidates 는 libcuda.so.1 의 후보 경로다.
func LibcudaCandidates() []string { return libCandidates("libcuda.so.1") }

// NcclLibCandidates 는 libnccl.so.2 의 후보 경로다.
func NcclLibCandidates() []string { return libCandidates("libnccl.so.2") }

// ResolveLibPath 는 라이브러리 경로를 확정한다. explicit (env/flag 지정값) 이 있으면 그대로 쓰고,
// 비어 있으면 후보를 순회해 첫 실존 경로를 돌려준다 (contention 의 podCgroupDir 후보 stat 패턴).
// 전부 없으면 빈 문자열이며 호출부가 순회 목록 로그와 graceful 비활성을 담당한다.
func ResolveLibPath(explicit string, candidates []string) string {
	// env/flag 입력의 우발적 공백을 정리해 반환한다. 공백 낀 값이 검사만 통과하고 attach 에서
	// 깨지는 비일관을 막는다.
	if trimmed := strings.TrimSpace(explicit); trimmed != "" {
		return trimmed
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}
