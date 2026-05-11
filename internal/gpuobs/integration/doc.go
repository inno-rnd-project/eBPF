// Package integration 은 gpuobs 의 컴포넌트 간 결합 동작을 envtest + in-process loopback 으로
// 검증하는 통합 테스트 모음이다. cuda Reader / podMap / visDev / metrics / kube.Resolver 가
// 단위 테스트만으로는 잡히지 않는 정합성 (NVML refresh 사이클의 podMap 일괄 적재 + RetainCudaSeries
// cleanup, dispatch hot path 의 캐시 hit/miss 분기, /metrics endpoint 의 Prometheus text format
// 정합성 등) 을 본 패키지에서 다룬다.
//
// 본 패키지의 모든 _test.go 파일은 //go:build integration build tag 로 보호되어 일반
// `go test ./...` 실행에는 포함되지 않으며, `make test-integration` 또는 `go test -tags=integration`
// 로만 실행된다. 이는 envtest binary (etcd + kube-apiserver) 다운로드 / boot 시간이 단위 테스트와
// 어울리지 않고, race detector 항상 on 상태에서 실행 시간 ceiling 60 초 안에 끝나도록 분리해
// 운용하기 위함이다.
//
// 비목표:
//   - 실 BPF kernel attach 의 e2e (별도 이슈, self-hosted runner 필요)
//   - 실 multi-GPU 노드의 attribution 정확도 회귀 (별도 이슈, multi-GPU 환경 필요)
//   - DCGM exporter 메트릭과의 의미 비교 (#35 시점)
package integration
