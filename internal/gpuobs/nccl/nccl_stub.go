//go:build !nccl

// nccl_stub.go는 build tag nccl이 비활성인 기본 빌드에서 컴파일되는 NewProduction stub이다.
// dev cluster의 RTX 3090 환경처럼 NCCL 데이터센터 GPU와 libnccl.so가 부재한 빌드에서는 uprobe
// 기반 production Profiler를 컴파일하지 않고 noop을 돌려준다. production 구현은 build tag nccl이
// 활성인 nccl_real.go에 있으며 cilium/ebpf의 uprobe_multi link와 ringbuf reader에 의존한다.
package nccl

// NewProduction은 기본 빌드에서 noop Profiler를 돌려준다. libPath와 nodeName 인자는 nccl_real.go의
// 동명 함수 시그니처와 정합을 위해 받되 stub에서는 사용하지 않는다. cmd/gpuobs-agent의 wire-up이
// build tag 무관하게 동일한 호출부를 유지하도록 시그니처를 일치시킨다.
func NewProduction(_, _ string) Profiler {
	return NewNoop()
}
