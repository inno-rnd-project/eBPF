// Package mps 는 NVIDIA Multi-Process Service (MPS) daemon 의 활성 여부를 OS 단에서 감지하는 모듈이다.
// NVML 이 MPS 분할 정보를 직접 노출하지 않아 (#104 비목표) per-process util 분할 정확도 trade-off 를
// 운영자가 인지할 수 있도록, 본 패키지가 감지한 활성 여부를 self-health 메트릭 gpuobs_mps_active 로
// 노출한다. MPS active 환경에서는 단일 CUDA context 가 다수 process 로 분할되므로 SM util 측정의
// per-process attribution 이 부정확해질 수 있다.
package mps

import (
	"os"
	"path/filepath"
)

// defaultControlPipe 는 MPS daemon 이 nvidia-cuda-mps-control 시작 시 생성하는 IPC pipe 디렉토리의
// upstream 기본 위치다. CUDA_MPS_PIPE_DIRECTORY 환경 변수로 override 가능하므로 detect 는 양쪽을 본다.
const defaultControlPipe = "/tmp/nvidia-mps"

// defaultLogDir 는 NVIDIA upstream 권장 위치 (/var/run/nvidia/mps) 다. systemd unit 또는 컨테이너
// MPS deployment 에서 흔히 사용되어 control pipe override 가 없어도 본 경로로 detect 된다.
const defaultLogDir = "/var/run/nvidia/mps"

// procFS 는 process scan 의 root 경로다. 본 패키지의 모든 file system 접근이 본 변수를 거치므로
// 테스트가 fake root 를 주입해 분기 검증 가능 하다.
var procFS = "/proc"

// fsRoot 는 control pipe / log dir 경로의 root 다. 테스트가 임시 디렉토리를 주입해 detect 분기를
// 결정적으로 검증 가능 하게 한다.
var fsRoot = ""

// Detect 는 다음 3종 신호의 OR 로 MPS daemon 활성 여부를 판정한다 — (1) CUDA_MPS_PIPE_DIRECTORY env 가
// 가리키는 경로 존재, (2) 기본 control pipe (/tmp/nvidia-mps) 또는 log dir (/var/run/nvidia/mps) 존재,
// (3) nvidia-cuda-mps-control process 의 /proc/<pid>/comm 매칭. 임의 한 신호라도 hit 이면 true.
// 본 함수는 호출 비용이 작아 collector pollOnce 마다 호출되도 dispatch latency 에 영향이 없다.
func Detect() bool {
	if checkPipeDirectory() {
		return true
	}
	if checkDefaultPaths() {
		return true
	}
	if checkProcess() {
		return true
	}
	return false
}

// checkPipeDirectory 는 CUDA_MPS_PIPE_DIRECTORY env 가 설정 되어 있으면 그 경로 의 디렉토리 존재 를
// 확인한다. env 미설정 또는 경로 부재는 모두 false.
func checkPipeDirectory() bool {
	dir := os.Getenv("CUDA_MPS_PIPE_DIRECTORY")
	if dir == "" {
		return false
	}
	info, err := os.Stat(applyRoot(dir))
	return err == nil && info.IsDir()
}

// checkDefaultPaths 는 NVIDIA upstream 기본 위치 2종 (/tmp/nvidia-mps, /var/run/nvidia/mps) 의 존재 를
// 확인한다. systemd unit 또는 컨테이너 MPS deployment 에서 흔히 사용 되는 경로 들이다.
func checkDefaultPaths() bool {
	for _, p := range []string{defaultControlPipe, defaultLogDir} {
		if info, err := os.Stat(applyRoot(p)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// checkProcess 는 /proc/<pid>/comm 을 순회 하며 nvidia-cuda-mps-control daemon process 가 활성 인지
// 확인한다. 단발 string compare 라 device 수가 적은 typical workload 에서 비용 무시 가능 하다.
func checkProcess() bool {
	entries, err := os.ReadDir(applyRoot(procFS))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !isAllDigits(name) {
			continue
		}
		commPath := filepath.Join(applyRoot(procFS), name, "comm")
		data, err := os.ReadFile(commPath)
		if err != nil {
			continue
		}
		comm := string(data)
		if len(comm) > 0 && comm[len(comm)-1] == '\n' {
			comm = comm[:len(comm)-1]
		}
		if comm == "nvidia-cuda-mps" || comm == "nvidia-cuda-mps-control" {
			return true
		}
	}
	return false
}

// applyRoot 는 fsRoot 가 설정 되어 있으면 path 를 그 아래로 매핑 해 반환 한다. 테스트가 임시 디렉토리
// 를 주입 해 detect 분기 를 격리 검증 가능 하게 하는 hook. filepath.Join 의 흡수 규칙 으로 절대 경로의
// 앞에 root 가 prepend 되지 않는 결함을 회피 하기 위해 절대경로의 선행 구분자를 명시 제거 한다.
func applyRoot(path string) string {
	if fsRoot == "" {
		return path
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(clean) {
		// "/proc" → "proc" 로 변환 해 filepath.Join(fsRoot, "proc") 가 fsRoot 아래 로 정상 매핑 되게 한다.
		clean = clean[1:]
	}
	return filepath.Join(fsRoot, clean)
}

// isAllDigits 는 /proc 의 PID 디렉토리 식별을 위한 helper.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
