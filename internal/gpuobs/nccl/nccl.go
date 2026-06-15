// Package nccl은 NVIDIA NCCL (NVIDIA Collective Communication Library) profiler 통합의 인터페이스
// skeleton이다. RTX 3090 단일 GPU 환경에서는 collective operation 자체가 발생하지 않으므로
// profiler attach의 실 가치 검증이 불가하다. 본 패키지는 인터페이스 정의와 noop 기본 구현
// 만 두고 실제 attach는 데이터센터 GPU (A100과 H100 등) 환경에서만 활성한다. NCCL profiler
// 자체가 cuda runtime의 cuProfilerStart/Stop 또는 NCCL_PROFILE env 기반으로 동작하므로
// runtime dlopen 또는 build tag 분리로 SDK 부재 환경의 빌드 깨짐을 회피한다.
//
// 본 패키지는 internal/gpuobs/nvml과 internal/gpuobs/dcgm과 동등 레벨의 leaf 패키지다.
// allreduce와 broadcast 등 collective operation의 wall-clock 분포와 rank별 wait 신호를
// 추적해 dominant cause classification의 신규 nccl_collective_stall cause slot의 score 산출
// 입력으로 활용한다.
package nccl

import "time"

// Event는 NCCL collective operation의 단일 sample 표현이다. Operation은 allreduce와
// broadcast 같은 collective 종류이고 DurationNs는 wall-clock 소요 시간 (nanoseconds) 이며
// RankCount는 본 collective에 참여한 rank 수다. Timestamp는 collective 종료 시각이다.
type Event struct {
	Operation  string
	DurationNs uint64
	RankCount  int
	Timestamp  time.Time
}

// Profiler는 NCCL collective event의 비동기 수집 추상 인터페이스다. production 구현은
// build tag nccl 분리한 파일에서 NCCL profiler callback 또는 cuProfiler symbol을 attach
// 한다. 본 패키지의 기본 구현은 noopProfiler라 graceful degradation으로 closed event
// channel만 노출한다.
type Profiler interface {
	// Available은 NCCL profiler가 정상 attach 가능한 상태인지 돌려준다. gpuobs_nccl_profiler_
	// available self-health gauge의 값 산출에 사용된다. noopProfiler는 항상 false를 돌려
	// 주어 dev cluster의 RTX 3090 환경에서 graceful degradation 식별 진입점이 된다.
	Available() bool

	// Attach는 NCCL profiler callback 또는 cuProfiler symbol을 attach한다. noopProfiler는
	// nil을 돌려주어 wire-up 흐름이 정상 진행된다.
	Attach() error

	// Events는 attach된 profiler가 emit하는 collective event channel을 돌려준다. noopProfiler
	// 는 closed channel을 돌려주어 호출자가 정상 range 종료한다.
	Events() <-chan Event

	// Close는 profiler detach와 리소스 정리 진입점이다. noopProfiler는 nil을 돌려준다.
	Close() error
}

// noopProfiler는 본 패키지의 기본 구현이다. NCCL SDK가 부재한 환경 또는 build tag nccl이
// 비활성인 빌드에서 사용된다. Events가 미리 close된 channel을 돌려주어 호출자의 range
// 루프가 정상 종료한다.
type noopProfiler struct {
	closed chan Event
}

// NewNoop은 noopProfiler의 factory다. 본 PR의 cmd/gpuobs-agent/main.go가 GPUOBS_NCCL_ENABLED
// env가 false (기본값) 일 때 본 factory로 인스턴스를 생성한다. closed channel을 미리 만들어
// Events 호출자가 nil channel block 위험 없이 즉시 range 종료 가능하다.
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
