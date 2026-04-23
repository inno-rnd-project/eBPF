package collector

import "context"

// Collector는 gpuobs의 수집 루프를 소유한다.
// Phase 1의 placeholder는 상태가 없으며, Phase 2에서 NVML 핸들과 폴링 주기가
// 필드로 추가된다.
type Collector struct{}

// New는 기본 Collector를 생성한다.
// Phase 2에서 NVML 인터페이스와 Config 의존성을 주입 받도록 시그니처가 확장된다.
func New() *Collector {
	return &Collector{}
}

// Run은 수집 루프를 실행한다.
// Phase 1 placeholder 동작은 다음과 같다.
//
//   - 진입 직후 onReady()를 호출해 readiness를 true로 전환시킴
//   - ctx가 취소될 때까지 블록
//   - 종료 시 nil 반환
//
// onReady는 nil이어도 안전하며, 그 경우 호출이 생략된다.
func (c *Collector) Run(ctx context.Context, onReady func()) error {
	if onReady != nil {
		onReady()
	}
	<-ctx.Done()
	return nil
}
