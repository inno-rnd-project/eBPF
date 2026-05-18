package correlation

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

// EnumeratePairs 는 입력 LabeledSeries 슬라이스에서 노드 한정 페어를 생성한다. 정책은 다음과 같다.
//
//   - 양쪽 모두 동일 node 라벨을 가진 시계열만 페어로 묶는다 (cross-node 제외). node 라벨이 비어
//     있는 entry 는 pair 후보에서 제외된다
//   - 양쪽 모두 동일 (src_namespace, src_pod) 인 경우 self-pair 라 제외한다. 같은 Pod 의 두 다른
//     metric 끼리는 self 가 아니라 metric pair 라 포함된다
//   - (X, Y) 와 (Y, X) 를 별도 페어로 둘 다 생성한다. 비대칭 분석 (X 자원이 Y latency 를 예측 vs
//     Y 자원이 X latency 를 예측) 을 위해서다
//
// 결과는 결정적 순서 (입력 순서 기반) 로 반환되어 테스트가 안정적으로 비교 가능하다.
func EnumeratePairs(items []LabeledSeries) []Pair {
	out := make([]Pair, 0, len(items)*len(items))
	for i, src := range items {
		srcNode := src.Series.Labels["node"]
		if srcNode == "" {
			continue
		}
		srcNS := src.Series.Labels["src_namespace"]
		srcPod := src.Series.Labels["src_pod"]
		for j, dst := range items {
			if i == j {
				// 같은 (metric, series) 자기 자신과는 비교하지 않는다.
				continue
			}
			dstNode := dst.Series.Labels["node"]
			if dstNode != srcNode {
				// cross-node 제외.
				continue
			}
			dstNS := dst.Series.Labels["src_namespace"]
			dstPod := dst.Series.Labels["src_pod"]
			if srcNS == dstNS && srcPod == dstPod && src.Metric == dst.Metric {
				// 같은 Pod 의 동일 metric (즉 동일 시계열의 중복) 만 self 로 본다. 동일 Pod 의 다른
				// metric 페어는 cause score 와 latency 의 self-correlation 처럼 운영 가치가 있어 유지.
				continue
			}
			out = append(out, Pair{
				Key: PairKey{
					SrcNamespace: srcNS,
					SrcPod:       srcPod,
					SrcMetric:    src.Metric,
					DstNamespace: dstNS,
					DstPod:       dstPod,
					DstMetric:    dst.Metric,
				},
				Src: src.Series,
				Dst: dst.Series,
			})
		}
	}
	return out
}
