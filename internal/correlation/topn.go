package correlation

import (
	"sort"
	"strings"
)

// ResourceDimension 은 noisy neighbor 메트릭의 resource_dimension 라벨에 들어가는 자원 종류다.
// 본 이슈 #51 의 메트릭 schema 가 cpu / memory / network / gpu 네 가지를 요구한다. latency 는
// dimension 이 아니라 victim 식별 기준이라 별도 상수로 두지 않는다 (latency 페어만 noisy neighbor
// 모델에 부합한다). 이슈 원안의 disk_io 는 본 시리즈가 disk 차원 cause score 를 도입하지 않아
// 정의 불가하므로 본 패키지에서 제외한다.
type ResourceDimension string

const (
	DimensionCPU     ResourceDimension = "cpu"
	DimensionMemory  ResourceDimension = "memory"
	DimensionNetwork ResourceDimension = "network"
	DimensionGPU     ResourceDimension = "gpu"
	// DimensionUnknown 은 metric 문자열에서 dimension 을 식별할 수 없을 때 반환된다. SelectTopN 은
	// 본 값을 가진 결과를 결과에서 제외해 카디널리티 폭발을 차단한다.
	DimensionUnknown ResourceDimension = ""
)

// PodIdentity 는 noisy neighbor 결과에서 victim 또는 suspect 한쪽을 가리키는 최소 식별자다. PairKey
// 가 두 Pod 를 한 struct 에 묶는 데 비해 NoisyNeighbor 는 victim 과 suspect 를 각각 별도 struct 로
// 분리해 운영자가 결과 해석 시 양쪽을 헷갈리지 않게 한다.
type PodIdentity struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	PodUID    string `json:"pod_uid"`
}

// NoisyNeighbor 는 한 victim 의 한 dimension 에서 채택된 단일 suspect 의 페어 정보다. exporter 가
// 본 struct 를 correlation_noisy_neighbor_score 와 correlation_noisy_neighbor_lag_seconds 두 메트릭
// 으로 변환한다.
type NoisyNeighbor struct {
	Victim        PodIdentity       `json:"victim"`
	VictimMetric  string            `json:"victim_metric"`
	Suspect       PodIdentity       `json:"suspect"`
	SuspectMetric string            `json:"suspect_metric"`
	Dimension     ResourceDimension `json:"dimension"`
	// VictimSignal 은 victim 영향 종착 차원 (#150 의 latency / throughput / error 와 #174 의 gpu) 이다.
	// dimension 이 suspect 의 자원 종류라면 VictimSignal 은 victim 측 품질 저하의 종류다. Top-N 은
	// (victim, victim_signal, dimension) 그룹별로 산정되어 한 victim 이 신호별로 독립 순위를 갖는다.
	VictimSignal VictimSignal `json:"victim_signal"`
	// Rank 는 (victim, victim_signal, dimension) 그룹 내 max_abs_value 내림차순 1-based 순위다. 1 이 가장 강한 상관.
	Rank        int     `json:"rank"`
	Score       float64 `json:"score"`
	LagSteps    int     `json:"lag_steps"`
	SampleCount int     `json:"sample_count"`
	// #69 Granger causality 결과. PValue 는 src 가 dst 를 Granger-cause 하는지의 통계적 유의성.
	// GrangerOK=false 면 표본 부족 또는 행렬 singular 로 산정이 자연 skip 되었다.
	PValue    float64 `json:"p_value"`
	GrangerOK bool    `json:"granger_ok"`
	// #146 effect size 결과. Impact 는 suspect 압박 시 victim latency 증가량 (seconds) 으로 간섭의
	// 절대 영향 크기다. Score (상관 강도) 와 독립된 지표라 운영자가 우선순위 판단에 함께 활용한다.
	// ImpactOK=false 면 표본 부족 등으로 산정이 skip 되어 Impact 는 0 이다. #175 부터 본 두 필드는
	// latency victim 전용 legacy 로 유지된다.
	Impact   float64 `json:"impact_seconds"`
	ImpactOK bool    `json:"impact_ok"`
	// #175 ImpactMagnitude 는 victim 신호별 native 단위 degradation 크기 (latency=seconds, throughput=
	// bytes/s 감소, error=drops/s 증가, gpu=util 감소) 로 latency 외 신호까지 확장된 effect size 다.
	// ImpactPValue 는 high / low 구간 평균 차이의 Welch t-test two-sided p-value 로 effect size 의 통계적
	// 유의성이다. *OK 가 false 면 표본 부족이나 구간 분산 0 등으로 산정이 graceful skip 된 상태다.
	ImpactMagnitude   float64 `json:"impact_magnitude"`
	ImpactMagnitudeOK bool    `json:"impact_magnitude_ok"`
	ImpactPValue      float64 `json:"impact_pvalue"`
	ImpactPValueOK    bool    `json:"impact_pvalue_ok"`
}

// classifyDimension 은 query 문자열에서 ResourceDimension 을 결정한다. 매칭 우선순위는 더 구체적인
// 키워드 (gpu) 가 더 일반적인 키워드 (memory) 보다 먼저 잡히도록 둔다. 예: pod:gpu_memory_utilization_ratio:5m
// 는 gpu 가 memory 보다 먼저 잡혀 DimensionGPU 로 분류된다.
//
// host_compute_stall_score 는 host CPU 가 GPU compute 를 stall 시키는 score 라 cpu 차원에 두며
// compute 키워드를 cpu case 에 포함한다.
func classifyDimension(metric string) ResourceDimension {
	switch {
	case strings.Contains(metric, "gpu"):
		return DimensionGPU
	case strings.Contains(metric, "network"):
		return DimensionNetwork
	case strings.Contains(metric, "memory"):
		return DimensionMemory
	case strings.Contains(metric, "cpu") || strings.Contains(metric, "compute"):
		return DimensionCPU
	}
	return DimensionUnknown
}

// isLatencyMetric 은 metric 이 latency 토큰을 포함하는지 보는 단순 헬퍼다. victim 신호 판정은
// classifyVictimSignal 로 일원화했으며 본 헬퍼는 단위 테스트의 보조 단정에만 남겨둔다.
func isLatencyMetric(metric string) bool {
	return strings.Contains(metric, "latency")
}

// VictimSignal 은 #150 의 victim 영향 종착 차원이다. 간섭이 latency p99 악화뿐 아니라 throughput 저하
// (netobs_pod_bytes_total 기반) 나 error율 증가 (drop 기반) 로도 나타나는 서비스 품질 저하를 victim
// 축에서 구분한다. #174 부터 GPU 사용률 저하 (pod:gpu_util_p95:5m 기반) 도 victim 신호로 추가되어
// 네트워크 간섭이 GPU 저하로 이어지는 경로를 직접 상관으로 노출한다. noisy neighbor 메트릭의
// victim_signal 라벨에 그대로 들어간다.
type VictimSignal string

const (
	SignalLatency    VictimSignal = "latency"
	SignalThroughput VictimSignal = "throughput"
	SignalError      VictimSignal = "error"
	// SignalGPU 는 #174 의 GPU victim 신호다. pod 단위 GPU 사용률 (pod:gpu_util_p95:5m) 저하를 victim
	// 품질 저하로 본다. 네트워크 간섭으로 GPU 워크로드가 starvation 되면 사용률이 떨어져 suspect 압박과
	// 음의 상관으로 나타나며 SelectTopN 은 max|corr| 로 부호 무관하게 포착한다. throughput / error 와
	// 동일하게 latency 전용 레이어 (dominant / cross-* / impact_seconds) 에서는 자연 제외된다.
	SignalGPU VictimSignal = "gpu"
	// SignalNone 은 victim 시계열이 아닌 (suspect cause score 등) 메트릭에 반환된다. SelectTopN 은
	// 페어 정확히 한쪽이 victim (signal != none) 이어야 채택한다.
	SignalNone VictimSignal = ""
)

// classifyVictimSignal 은 query 문자열에서 VictimSignal 을 결정한다. "bytes" / "drop" 같은 일반 토큰
// 대신 victim 의 실제 netobs source 메트릭 이름 (stage_latency / netobs_pod_bytes_total /
// netobs_drop_events_flow_total) 으로 매칭한다. 일반 토큰을 쓰면 운영자가 ExtraMetrics 로 추가한 커스텀
// suspect (예: container_network_receive_bytes_total, container_memory_working_set_bytes) 가 "bytes"
// 때문에 victim 으로 오분류되어 suspect 가 분석에서 빠지는 버그가 생긴다. source 메트릭 이름으로 좁히면
// suspect cause score (network_throughput_score / network_retrans_score 등) 와 임의 커스텀 메트릭이
// 모두 SignalNone 으로 분류되어 suspect 로 정상 취급된다.
func classifyVictimSignal(metric string) VictimSignal {
	switch {
	case strings.Contains(metric, "stage_latency"):
		return SignalLatency
	case strings.Contains(metric, "netobs_pod_bytes_total"):
		return SignalThroughput
	case strings.Contains(metric, "netobs_drop_events_flow_total"):
		return SignalError
	// #174 GPU victim 신호. pod 단위 GPU 사용률 recording rule 명 pod:gpu_util 로 매칭한다. gpu_util
	// 단독 토큰은 운영자가 ExtraMetrics 로 추가한 커스텀 GPU suspect (container_gpu_utilization /
	// node_gpu_utilization 등) 까지 victim 으로 오분류해 suspect 분석에서 빠뜨리므로, pod: 접두사를
	// 포함한 source 메트릭 이름으로 좁힌다 (위 latency / throughput / error 와 동일 원칙). suspect 인
	// pod:gpu_memory_utilization_ratio 는 pod:gpu_util 을 포함하지 않아 (pod:gpu_memory...) SignalNone
	// 으로 남아 suspect 로 정상 취급된다.
	case strings.Contains(metric, "pod:gpu_util"):
		return SignalGPU
	}
	return SignalNone
}

// isVictimMetric 은 metric 이 victim 시계열 (어떤 signal 이든) 인지 반환한다. enumerate / select 단계
// 에서 suspect (cause) 와 victim (outcome) 을 가르는 단정 기준으로, suspect = victim 이 아님이다.
func isVictimMetric(metric string) bool {
	return classifyVictimSignal(metric) != SignalNone
}

// SelectTopN 은 Correlate 결과에서 noisy neighbor 페어를 추출한다. 다음 규칙을 단정적으로 적용한다.
//
//   - #150 페어 정확히 한쪽이 victim 메트릭 (latency / throughput / error) 이고 반대쪽이 victim 이 아닌
//     cause score 인 페어만 채택한다. 양쪽 모두 victim 이거나 양쪽 모두 cause 인 페어는 noisy neighbor
//     모델에 부합하지 않아 제외한다. victim signal 은 classifyVictimSignal 로 판정하며 suspect cause
//     score 명과 토큰이 겹치지 않아 dimension 분류 정합이 유지된다.
//   - 채택된 페어 중 Src 가 suspect (cause), Dst 가 victim 인 방향만 사용한다. 반대 방향 (EnumeratePairs
//     가 만드는 (Y,X)) 은 같은 (victim, suspect) 가 두 번 등장하지 않도록 자동 dedup 의미로 제외한다.
//   - Status 가 StatusOK 또는 StatusPartial 인 결과만 채택한다. SkippedConstant / SkippedLowSamples
//     는 의미 있는 score 가 없어 제외한다.
//   - suspect 메트릭에서 dimension 을 분류해 DimensionUnknown 이면 제외한다 (카디널리티 가드).
//   - (victim_namespace, victim_pod, victim_pod_uid, victim_signal, dimension) 그룹별 max_abs_value
//     내림차순으로 정렬해 상위 topN 개에 rank 1..topN 부여한다. 동률은 (suspect_namespace, suspect_pod,
//     suspect_pod_uid) 라벨 lexicographic 순서로 결정한다. victim 이 신호별로 독립 Top-N 을 갖는다.
//   - legacy effect size (impact_seconds) 는 latency victim 에만 유효해 throughput / error / gpu victim
//     은 ImpactOK 를 false 로 둔다. #175 의 impact_magnitude 와 impact_pvalue 는 victim 신호 방향을
//     반영해 전 신호에서 산출되며 candidate 가 그대로 carry 한다.
//
// 결과는 (victim_namespace, victim_pod, victim_signal, dimension, rank) 순으로 정렬된 슬라이스다. 동일
// cycle 의 재현성을 보장한다. topN <= 0 이면 nil 을 반환한다.
func SelectTopN(results []CorrelationResult, topN int) []NoisyNeighbor {
	if topN <= 0 {
		return nil
	}

	type candidate struct {
		victim        PodIdentity
		victimMetric  string
		victimSignal  VictimSignal
		suspect       PodIdentity
		suspectMetric string
		dimension     ResourceDimension
		score         float64
		lag           int
		samples       int
		// #69 Granger causality 결과. GrangerOK=false 면 pvalue=0 으로 노출되고 Collector emit 단계
		// 에서 pvalue 시리즈 자체가 skip 된다. dedup 의 max score 비교는 score 만 본다 (정확 동률 시
		// GrangerOK 우선 채택은 본 시리즈 scope 외이며 map iteration 순서에 의존하므로 결정적이지
		// 않다).
		pvalue    float64
		grangerOK bool
		// #146 / #175 effect size. Score (상관 강도) 와 독립이라 dedup 의 max score 비교에는 영향을 주지
		// 않고 채택된 candidate 의 값을 그대로 따라간다. impact / impactOK 는 latency 전용 legacy 이고
		// impactMagnitude / impactPValue 는 전 신호 확장이다.
		impact            float64
		impactOK          bool
		impactMagnitude   float64
		impactMagnitudeOK bool
		impactPValue      float64
		impactPValueOK    bool
	}

	candidates := make([]candidate, 0, len(results))
	for _, r := range results {
		if r.Status != StatusOK && r.Status != StatusPartial {
			continue
		}
		// #150 victim 판정 다차원화. 페어 정확히 한쪽이 victim (latency / throughput / error) 이고
		// 반대쪽이 victim 이 아닌 cause score 여야 한다. src 가 victim 인 역방향은 같은 (victim, suspect)
		// 중복 회피를 위해 skip 해 suspect→victim 방향만 채택한다.
		srcSignal := classifyVictimSignal(r.Pair.SrcMetric)
		dstSignal := classifyVictimSignal(r.Pair.DstMetric)
		if (srcSignal == SignalNone) == (dstSignal == SignalNone) {
			continue
		}
		if srcSignal != SignalNone {
			continue
		}
		victimSignal := dstSignal
		dim := classifyDimension(r.Pair.SrcMetric)
		if dim == DimensionUnknown {
			continue
		}
		// same-pod cross-metric pair 는 noisy neighbor 모델 (이웃 Pod 의 자원 압박) 에 부합하지
		// 않는다. EnumeratePairs 가 동일 Pod 의 두 다른 metric series 도 페어로 만들어 victim 의
		// 자기 자신이 suspect rank 1 을 차지할 수 있어 본 단계에서 명시 제외한다. PodUID 가 있으면
		// UID 기준이 가장 정확하고 둘 다 비어 있으면 namespace/pod 로 보수적 비교한다.
		if r.Pair.SrcPodUID != "" && r.Pair.DstPodUID != "" {
			if r.Pair.SrcPodUID == r.Pair.DstPodUID {
				continue
			}
		} else if r.Pair.SrcNamespace == r.Pair.DstNamespace && r.Pair.SrcPod == r.Pair.DstPod {
			continue
		}
		candidates = append(candidates, candidate{
			victim: PodIdentity{
				Namespace: r.Pair.DstNamespace,
				Pod:       r.Pair.DstPod,
				PodUID:    r.Pair.DstPodUID,
			},
			victimMetric: r.Pair.DstMetric,
			victimSignal: victimSignal,
			suspect: PodIdentity{
				Namespace: r.Pair.SrcNamespace,
				Pod:       r.Pair.SrcPod,
				PodUID:    r.Pair.SrcPodUID,
			},
			suspectMetric: r.Pair.SrcMetric,
			dimension:     dim,
			score:         r.MaxAbsValue,
			lag:           r.MaxAbsLag,
			samples:       r.SampleCount,
			pvalue:        r.PValue,
			grangerOK:     r.GrangerOK,
			impact:        r.Impact,
			// #146 impact_seconds 는 latency 증가량 (seconds) 의미라 latency victim 에만 유효하다.
			// throughput / error / gpu victim 은 단위가 달라 impact_seconds 로 노출하지 않도록 gate 하고,
			// 대신 #175 의 impactMagnitude (native 단위) 로 전 신호 영향 크기를 노출한다.
			impactOK:          r.ImpactOK && victimSignal == SignalLatency,
			impactMagnitude:   r.ImpactMagnitude,
			impactMagnitudeOK: r.ImpactMagnitudeOK,
			impactPValue:      r.ImpactPValue,
			impactPValueOK:    r.ImpactPValueOK,
		})
	}

	// 한 suspect 가 같은 dimension 에 매핑되는 여러 metric (예: cpu_throttle_score 와
	// host_compute_stall_score 둘 다 cpu) 로 여러 candidate 를 만드는 경우가 있다. Top-N 이 metric
	// 수가 아닌 unique noisy-neighbor Pod 수를 세야 하므로 (victim, suspect, dimension) 단위로
	// max score 를 가진 candidate 하나만 채택해 dedup 한다.
	type pairKey struct {
		victimNamespace  string
		victimPod        string
		victimPodUID     string
		victimSignal     VictimSignal
		suspectNamespace string
		suspectPod       string
		suspectPodUID    string
		dimension        ResourceDimension
	}
	byPair := make(map[pairKey]candidate)
	for _, c := range candidates {
		k := pairKey{
			victimNamespace:  c.victim.Namespace,
			victimPod:        c.victim.Pod,
			victimPodUID:     c.victim.PodUID,
			victimSignal:     c.victimSignal,
			suspectNamespace: c.suspect.Namespace,
			suspectPod:       c.suspect.Pod,
			suspectPodUID:    c.suspect.PodUID,
			dimension:        c.dimension,
		}
		if prev, ok := byPair[k]; !ok || c.score > prev.score {
			byPair[k] = c
		}
	}
	deduped := make([]candidate, 0, len(byPair))
	for _, c := range byPair {
		deduped = append(deduped, c)
	}

	type groupKey struct {
		namespace    string
		pod          string
		podUID       string
		victimSignal VictimSignal
		dimension    ResourceDimension
	}
	groups := make(map[groupKey][]candidate)
	for _, c := range deduped {
		k := groupKey{c.victim.Namespace, c.victim.Pod, c.victim.PodUID, c.victimSignal, c.dimension}
		groups[k] = append(groups[k], c)
	}

	groupKeys := make([]groupKey, 0, len(groups))
	for k := range groups {
		groupKeys = append(groupKeys, k)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		a, b := groupKeys[i], groupKeys[j]
		if a.namespace != b.namespace {
			return a.namespace < b.namespace
		}
		if a.pod != b.pod {
			return a.pod < b.pod
		}
		if a.podUID != b.podUID {
			return a.podUID < b.podUID
		}
		if a.victimSignal != b.victimSignal {
			return a.victimSignal < b.victimSignal
		}
		return a.dimension < b.dimension
	})

	out := make([]NoisyNeighbor, 0)
	for _, gk := range groupKeys {
		cands := groups[gk]
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].score != cands[j].score {
				return cands[i].score > cands[j].score
			}
			if cands[i].suspect.Namespace != cands[j].suspect.Namespace {
				return cands[i].suspect.Namespace < cands[j].suspect.Namespace
			}
			if cands[i].suspect.Pod != cands[j].suspect.Pod {
				return cands[i].suspect.Pod < cands[j].suspect.Pod
			}
			return cands[i].suspect.PodUID < cands[j].suspect.PodUID
		})
		limit := topN
		if len(cands) < limit {
			limit = len(cands)
		}
		for i := 0; i < limit; i++ {
			c := cands[i]
			out = append(out, NoisyNeighbor{
				Victim:            c.victim,
				VictimMetric:      c.victimMetric,
				VictimSignal:      c.victimSignal,
				Suspect:           c.suspect,
				SuspectMetric:     c.suspectMetric,
				Dimension:         c.dimension,
				Rank:              i + 1,
				Score:             c.score,
				LagSteps:          c.lag,
				SampleCount:       c.samples,
				PValue:            c.pvalue,
				GrangerOK:         c.grangerOK,
				Impact:            c.impact,
				ImpactOK:          c.impactOK,
				ImpactMagnitude:   c.impactMagnitude,
				ImpactMagnitudeOK: c.impactMagnitudeOK,
				ImpactPValue:      c.impactPValue,
				ImpactPValueOK:    c.impactPValueOK,
			})
		}
	}
	return out
}
