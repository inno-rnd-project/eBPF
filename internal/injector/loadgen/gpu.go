package loadgen

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// gpuStressMemMiB 는 stress Pod 가 점유할 GPU 메모리 (MiB) 다. CUDA busy kernel 의 작업 버퍼이자
// "GPU 메모리 점유" 차원의 압박을 동시에 만든다. 공유 GPU (dev RTX 3090 24GiB) 에서 다른 워크로드를
// 밀어내지 않도록 보수적으로 둔다. 메모리 점유량 자체를 운영자 입력으로 노출하는 것은 follow-up.
const gpuStressMemMiB = 1024

// gpuTimeoutGraceSec 는 Duration 외에 CUDA init / cudaMalloc 여유와 hang 방지 belt 로 timeout 명령에
// 더하는 초다. 부하 프로그램 자체도 Duration 후 자동 종료하므로 정상 경로에서는 도달하지 않는다.
const gpuTimeoutGraceSec = 30

// gpuStressSource 는 컨테이너 안에서 nvcc 로 즉석 컴파일되는 CUDA 부하 프로그램이다. 인자는
// (점유율 percent, duration 초, 점유 메모리 MiB) 이며, busy kernel 한 사이클의 실행 시간을 측정해
// off 구간을 sleep 하는 duty cycle 로 단일 GPU 의 SM 점유율을 목표 percent 로 근사한다. runtime
// 이미지에는 nvcc 가 없어 devel 이미지를 쓰며, -arch=native 로 컨테이너에 할당된 GPU 의 compute
// capability 를 컴파일 시점에 자동 감지해 빌드한다. 컴파일이 대상 GPU 노드 위에서 일어나고 Pod 가
// nvidia.com/gpu 를 할당받아 드라이버가 보이므로, RTX 3090 (sm_86) 외 T4 / V100 / Hopper 등 다른
// 아키텍처 노드에서도 별도 수정 없이 정합한다.
const gpuStressSource = `#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <unistd.h>
#include <cuda_runtime.h>

__global__ void burn(float *buf, long n, int iters) {
    long idx = (long)blockIdx.x * blockDim.x + threadIdx.x;
    if (idx >= n) return;
    float v = buf[idx];
    for (int k = 0; k < iters; ++k) {
        v = fmaf(v, 1.0000001f, 0.0000001f);
    }
    buf[idx] = v;
}

static double now_sec() {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec * 1e-9;
}

int main(int argc, char **argv) {
    int pct = (argc > 1) ? atoi(argv[1]) : 80;
    int dur = (argc > 2) ? atoi(argv[2]) : 60;
    long memMiB = (argc > 3) ? atol(argv[3]) : 1024;
    if (pct < 1) pct = 1;
    if (pct > 100) pct = 100;
    if (dur < 1) dur = 1;

    long n = memMiB * 1024L * 1024L / (long)sizeof(float);
    if (n < 1024L) n = 1024L;

    float *buf = NULL;
    cudaError_t err = cudaMalloc((void **)&buf, (size_t)n * sizeof(float));
    if (err != cudaSuccess) {
        fprintf(stderr, "cudaMalloc(%ld floats) failed: %s\n", n, cudaGetErrorString(err));
        return 1;
    }
    cudaMemset(buf, 0, (size_t)n * sizeof(float));

    int threads = 256;
    long want = (n + threads - 1) / threads;
    int blocks = (want > 65535L) ? 65535 : (int)want;
    int iters = 100000;

    printf("gpuload start pct=%d dur=%ds mem=%ldMiB n=%ld blocks=%d threads=%d\n",
           pct, dur, memMiB, n, blocks, threads);
    fflush(stdout);

    double start = now_sec();
    long cycles = 0;
    while (now_sec() - start < (double)dur) {
        double t0 = now_sec();
        burn<<<blocks, threads>>>(buf, n, iters);
        err = cudaDeviceSynchronize();
        if (err != cudaSuccess) {
            fprintf(stderr, "kernel sync failed: %s\n", cudaGetErrorString(err));
            cudaFree(buf);
            return 1;
        }
        double on = now_sec() - t0;
        if (pct < 100) {
            double off = on * (100.0 - (double)pct) / (double)pct;
            if (off > 0.0) usleep((useconds_t)(off * 1e6));
        }
        cycles++;
    }
    cudaFree(buf);
    printf("gpuload done cycles=%ld elapsed=%.1fs\n", cycles, now_sec() - start);
    fflush(stdout);
    return 0;
}
`

// gpuGen 은 NVIDIA CUDA devel 컨테이너를 target node 에 spawn 해 CUDA busy kernel 로 실제 GPU 점유를
// 발생시킨다. 컨테이너 시작 시 nvcc 로 부하 프로그램을 컴파일한 뒤 duty cycle 로 목표 점유율을
// 근사하고 GPU 메모리도 함께 점유한다. NVIDIA GPU Operator 가 device plugin 을 설치한 노드에서만
// 동작하며 미설치 노드에서는 Pod 가 Pending 으로 남는다.
type gpuGen struct {
	client  kubernetes.Interface
	mu      sync.Mutex
	spawned []types.NamespacedName
}

func (g *gpuGen) Kind() Kind { return KindGPU }

// Start 는 stress-gpu-<target_pod> Pod 를 spawn 한다. Intensity 는 1~100 목표 점유율 percent 이며
// (비어 있으면 80) CUDA busy kernel 의 duty cycle 로 매핑된다. 단일 GPU device 를 요청하고 점유율로
// 부하 강도를 조절하므로 multi-GPU 환경이 아니어도 동작한다. busy loop 는 Duration 후 프로그램이
// 스스로 종료하며 timeout 명령이 추가 belt 로 hang 을 차단한다.
func (g *gpuGen) Start(ctx context.Context, params Params) error {
	if params.TargetNode == "" {
		return fmt.Errorf("gpu loadgen: target node is empty")
	}
	raw := strings.TrimSpace(params.Intensity)
	if raw == "" {
		raw = "80"
	}
	pct, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("gpu loadgen: parse intensity %q as utilization percent: %w", params.Intensity, err)
	}
	if pct < 1 || pct > 100 {
		return fmt.Errorf("gpu loadgen: utilization percent must be 1..100, got %d", pct)
	}
	durSec := int(params.Duration.Seconds())
	if durSec < 1 {
		durSec = 1
	}
	timeoutSec := durSec + gpuTimeoutGraceSec

	// nvcc 로 즉석 컴파일 후 timeout 으로 감싼 부하 프로그램을 exec 한다. heredoc delimiter 를 따옴표로
	// 감싸 CUDA 소스 안의 $ / 백틱이 shell 확장되지 않게 한다.
	script := "set -e\n" +
		"cat > /tmp/gpuload.cu <<'CUDA_EOF'\n" +
		gpuStressSource +
		"CUDA_EOF\n" +
		"nvcc -O3 -arch=native -o /tmp/gpuload /tmp/gpuload.cu\n" +
		fmt.Sprintf("exec timeout %d /tmp/gpuload %d %d %d\n", timeoutSec, pct, durSec, gpuStressMemMiB)

	name := sanitizeName("stress-gpu", params.TargetPod)
	meta := commonPodMeta(name, params)
	meta.Labels["injector.kind"] = string(KindGPU)
	pod := &corev1.Pod{
		ObjectMeta: meta,
		Spec: corev1.PodSpec{
			NodeName:      params.TargetNode,
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "cuda-stress",
					Image:   "nvidia/cuda:12.4.0-devel-ubuntu22.04",
					Command: []string{"sh", "-c", script},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
							"nvidia.com/gpu":      resource.MustParse("1"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
							"nvidia.com/gpu":      resource.MustParse("1"),
						},
					},
				},
			},
		},
	}
	if err := createPod(ctx, g.client, pod); err != nil {
		return err
	}
	g.mu.Lock()
	g.spawned = append(g.spawned, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name})
	g.mu.Unlock()
	return nil
}

func (g *gpuGen) Stop(ctx context.Context) error {
	g.mu.Lock()
	spawned := append([]types.NamespacedName(nil), g.spawned...)
	g.spawned = nil
	g.mu.Unlock()
	return deleteAll(ctx, g.client, spawned)
}

func (g *gpuGen) SpawnedPods() []types.NamespacedName {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]types.NamespacedName(nil), g.spawned...)
}
