# internal/correlation

이 패키지는 netobs와 gpuobs가 각각 발행하는 Prometheus 지표를 같은 조인 키(`node`, `src_namespace`, `src_pod`, `src_pod_uid`)로 묶어 네트워크 병목과 GPU 병목을 한 차원에서 함께 분석할 수 있도록 하기 위한 자리이다. 현재는 의도적으로 비어 있으며, 아래 착수 조건이 모두 충족된 뒤에 별도 이슈에서 구현을 시작한다.

## 착수 조건

- [ ] gpuobs Phase 3이 완료되어 `gpuobs_pod_*` 지표가 안정적으로 발행됨
- [ ] `netobs_pod_stage_*`와 `gpuobs_pod_*`의 라벨 키 집합이 확정되어 추후 변경 부담이 낮음
- [ ] Grafana/PromQL에서 두 지표 집합을 실제로 join하는 샘플 쿼리 검증

## 관련 이슈

- 상위 설계 이슈 #7 (GPU 관측 에이전트 분리 도입 설계)
- 후속 구현 이슈 `feat(correlation): netobs × gpuobs 조인 지표 도입` (착수 조건 충족 후 별도로 open)

## 현재 상태

`doc.go` 한 개 파일만 존재하며 공개 API는 없다. 다른 어떤 패키지에서도 import되지 않으며, 착수 시점에 이 디렉토리가 구현의 홈이 된다.
