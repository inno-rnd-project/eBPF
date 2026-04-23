// Package metrics는 gpuobs 에이전트가 Prometheus로 발행하는 지표를 정의한다.
// Phase 1에서는 에이전트 기동 여부를 알리는 최소한의 info Gauge만 등록되며,
// device 단위 Gauge 5종은 Phase 2에서 추가된다. gpuobs 전용 프리픽스 `gpuobs_`를
// 사용해 netobs 지표(`netobs_*`)와 네임스페이스를 분리한다.
package metrics
