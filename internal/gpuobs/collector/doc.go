// Package collector는 gpuobs의 수집 루프를 정의한다.
// Phase 1에서는 실제 NVML 폴링 없이 기동 직후 ready 신호만 내고 context 취소까지
// 대기하는 placeholder 동작만 수행한다. Phase 2에서 NVML 인터페이스와 ticker 기반
// 폴링 루프가 이 자리에 들어온다.
package collector
