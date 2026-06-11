// Package nccl 은 NVIDIA NCCL (NVIDIA Collective Communication Library) profiler 통합 의 인터페이스
// skeleton 이다. RTX 3090 단일 GPU 환경 에서는 collective operation 자체 가 발생 하지 않 으므로
// profiler attach 의 실 가치 검증 이 불가 하다. 본 패키지 는 인터페이스 정의 와 noop 기본 구현
// 만 두고 실제 attach 는 데이터센터 GPU (A100 과 H100 등) 환경 에서만 활성 한다. NCCL profiler
// 자체 가 cuda runtime 의 cuProfilerStart/Stop 또는 NCCL_PROFILE env 기반 으로 동작 하므로
// runtime dlopen 또는 build tag 분리 로 SDK 부재 환경 의 빌드 깨짐 을 회피 한다.
//
// 본 패키지 는 internal/gpuobs/nvml 과 internal/gpuobs/dcgm 과 동등 레벨 의 leaf 패키지 다.
// allreduce 와 broadcast 등 collective operation 의 wall-clock 분포 와 rank 별 wait 신호 를
// 추적 해 dominant cause classification 의 신규 nccl_collective_stall cause slot 의 score 산출
// 입력 으로 활용 한다.
package nccl

import "time"

// Event 는 NCCL collective operation 의 단일 sample 표현 이다. Operation 은 allreduce 와
// broadcast 같은 collective 종류 이고 DurationNs 는 wall-clock 소요 시간 (nanoseconds) 이며
// RankCount 는 본 collective 에 참여 한 rank 수 다. Timestamp 는 collective 종료 시각 이다.
type Event struct {
	Operation  string
	DurationNs uint64
	RankCount  int
	Timestamp  time.Time
}

// Profiler 는 NCCL collective event 의 비동기 수집 추상 인터페이스 다. production 구현 은
// build tag nccl 분리 한 파일 에서 NCCL profiler callback 또는 cuProfiler symbol 을 attach
// 한다. 본 패키지 의 기본 구현 은 noopProfiler 라 graceful degradation 으로 closed event
// channel 만 노출 한다.
type Profiler interface {
	// Available 은 NCCL profiler 가 정상 attach 가능 한 상태 인지 돌려 준다. gpuobs_nccl_profiler_
	// available self-health gauge 의 값 산출 에 사용 된다. noopProfiler 는 항상 false 를 돌려
	// 주어 dev cluster 의 RTX 3090 환경 에서 graceful degradation 식별 진입점 이 된다.
	Available() bool

	// Attach 는 NCCL profiler callback 또는 cuProfiler symbol 을 attach 한다. noopProfiler 는
	// nil 을 돌려 주어 wire-up 흐름 이 정상 진행 된다.
	Attach() error

	// Events 는 attach 된 profiler 가 emit 하는 collective event channel 을 돌려 준다. noopProfiler
	// 는 closed channel 을 돌려 주어 호출 자 가 정상 range 종료 한다.
	Events() <-chan Event

	// Close 는 profiler detach 와 리소스 정리 진입점 이다. noopProfiler 는 nil 을 돌려 준다.
	Close() error
}

// noopProfiler 는 본 패키지 의 기본 구현 이다. NCCL SDK 가 부재 한 환경 또는 build tag nccl 이
// 비활성 인 빌드 에서 사용 된다. Events 가 미리 close 된 channel 을 돌려 주어 호출 자 의 range
// 루프 가 정상 종료 한다.
type noopProfiler struct {
	closed chan Event
}

// NewNoop 은 noopProfiler 의 factory 다. 본 PR 의 cmd/gpuobs-agent/main.go 가 GPUOBS_NCCL_ENABLED
// env 가 false (기본값) 일 때 본 factory 로 인스턴스 를 생성 한다. closed channel 을 미리 만들어
// Events 호출 자 가 nil channel block 위험 없이 즉시 range 종료 가능 하다.
func NewNoop() Profiler {
	ch := make(chan Event)
	close(ch)
	return &noopProfiler{closed: ch}
}

func (*noopProfiler) Available() bool {
	return false
}

func (*noopProfiler) Attach() error {
	return nil
}

func (p *noopProfiler) Events() <-chan Event {
	return p.closed
}

func (*noopProfiler) Close() error {
	return nil
}
