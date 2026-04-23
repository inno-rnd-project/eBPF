// Package nvml은 NVIDIA Management Library 접근을 위한 gpuobs의 추상 인터페이스를 제공한다.
// Phase 1에서는 인터페이스만 선언하며, 실제 go-nvml 기반 dlopen 구현은 Phase 2에서
// 이 인터페이스를 만족하는 형태로 추가된다. 이 분리는 단위 테스트가 NVML을 mock으로
// 대체해 non-GPU CI 환경에서도 collector 동작을 검증할 수 있게 하려는 목적이다.
package nvml
