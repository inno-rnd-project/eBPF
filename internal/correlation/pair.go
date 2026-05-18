package correlation

import "sort"

// LabeledSeries 는 fetcher 가 반환하는 단위로 (metric 이름, 라벨, 시계열) 트리플이다. pair
// enumeration 이 metric 별 시계열을 노드와 Pod 기준으로 묶어 페어를 만들 때 본 자료를 입력으로
// 받는다.
type LabeledSeries struct {
	// Metric 은 Prometheus query 식별자 (예: "pod:cpu_throttle_score:5m") 다. 동일 metric 의 여러
	// 시계열 (다른 Pod) 이 본 슬라이스의 별개 entry 로 들어온다.
	Metric string
	// Series 는 Labels (node / src_namespace / src_pod / src_pod_uid 등) 와 Samples 를 보유한다.
	Series TimeSeries
}

// Pair 는 상관계수 산출 대상이 되는 두 LabeledSeries 의 참조다. PearsonWithLag 의 입력으로 그대로
// 전달 가능하도록 Src / Dst 두 시계열을 직접 보유한다.
type Pair struct {
	Key PairKey
	Src TimeSeries
	Dst TimeSeries
}

// EnumeratePairs 는 입력 LabeledSeries 슬라이스에서 노드 한정 Pod 페어를 생성한다. 정책은 다음과
// 같다.
//
//   - 양쪽 모두 동일 node 라벨을 가진 시계열만 페어로 묶는다 (cross-node 제외)
//   - node / src_namespace / src_pod 세 라벨이 모두 채워진 시계열만 Pod 페어 후보로 인정한다.
//     node-level 메트릭 (예: node:gpu_idle:5m 처럼 namespace / pod 라벨이 없는 series) 은 본 패키지의
//     Pod-pair 산출 schema 와 불일치라 자동 제외되어 빈 Pod 라벨로 emit 되는 false-positive 결과를
//     차단한다
//   - 양쪽 모두 동일 (src_namespace, src_pod, metric) 인 경우 self-pair 라 제외한다. 같은 Pod 의
//     두 다른 metric 끼리는 self 가 아니라 metric pair 라 포함된다
//   - (X, Y) 와 (Y, X) 를 별도 페어로 둘 다 생성한다. 비대칭 분석 (X 자원이 Y latency 를 예측 vs
//     Y 자원이 X latency 를 예측) 을 위해서다
//
// 구현은 노드별 그룹화 후 각 그룹 내에서만 중첩 루프를 돌려 cross-product 비용을 O(N^2) 에서
// Σ O(N_node^2) 로 감축한다. 노드 키는 정렬 순회해 출력 순서가 결정적이며 단위 테스트가 안정적
// 으로 비교 가능하다.
//
// 사전 할당 슬라이스 cap 은 두지 않는다 (입력 시계열 수의 제곱에 해당하는 cap 은 대형 cluster 에서
// OOM 위험).
func EnumeratePairs(items []LabeledSeries) []Pair {
	// 1단계: node 키 기준으로 그룹화. 라벨 완비 가드도 같이 적용해 후보 시계열만 그룹에 모은다.
	byNode := make(map[string][]LabeledSeries)
	for _, item := range items {
		node := item.Series.Labels["node"]
		ns := item.Series.Labels["src_namespace"]
		pod := item.Series.Labels["src_pod"]
		if node == "" || ns == "" || pod == "" {
			continue
		}
		byNode[node] = append(byNode[node], item)
	}

	// 노드 키 정렬 (Go map 순회는 비결정적이라 출력 순서 안정성 확보).
	nodeKeys := make([]string, 0, len(byNode))
	for k := range byNode {
		nodeKeys = append(nodeKeys, k)
	}
	sort.Strings(nodeKeys)

	// 2단계: 노드별 N_node^2 페어 enumerate.
	out := make([]Pair, 0)
	for _, node := range nodeKeys {
		group := byNode[node]
		for i, src := range group {
			srcNS := src.Series.Labels["src_namespace"]
			srcPod := src.Series.Labels["src_pod"]
			srcUID := src.Series.Labels["src_pod_uid"]
			for j, dst := range group {
				if i == j {
					continue
				}
				dstNS := dst.Series.Labels["src_namespace"]
				dstPod := dst.Series.Labels["src_pod"]
				if srcNS == dstNS && srcPod == dstPod && src.Metric == dst.Metric {
					continue
				}
				out = append(out, Pair{
					Key: PairKey{
						SrcNamespace: srcNS,
						SrcPod:       srcPod,
						SrcPodUID:    srcUID,
						SrcMetric:    src.Metric,
						DstNamespace: dstNS,
						DstPod:       dstPod,
						DstPodUID:    dst.Series.Labels["src_pod_uid"],
						DstMetric:    dst.Metric,
					},
					Src: src.Series,
					Dst: dst.Series,
				})
			}
		}
	}
	return out
}
