// Package correlation은 netobs와 gpuobs가 각각 발행하는 Prometheus 지표를
// 같은 조인 키(node, src_namespace, src_pod, src_pod_uid)로 묶어
// 네트워크 병목과 GPU 병목을 한 차원에서 분석하기 위한 코드이다.
//
// 현재는 의도적으로 비어 있으며, 데이터 수집 조건이 모두 충족된 뒤에 별도 이슈에서 구현을 시작한다.
// 착수 조건과 계획은 같은 디렉토리의 README.md를 참고한다. (이슈 #7 참고)
package correlation
