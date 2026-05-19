// Package blastradius 는 workload-injector 가 부하 시작 전 baseline 과 부하 윈도우 동안의 impact
// 두 시계열을 비교해 correlation_blast_radius_score (0 ~ 1) 를 산출하는 stateless 라이브러리다.
// 라이브러리는 K8s / Prometheus 의존성이 없으며 입력 슬라이스 두 개만으로 점수를 계산해 단위
// 테스트 가능성이 높다.
package blastradius

import (
	"math"
	"sort"
)

// Status 는 단일 victim 의 blast radius 산출 결과 분류다. exporter 가 메트릭 라벨로 본 값을 그대로
// 사용 가능하도록 string enum 으로 둔다.
type Status string

const (
	// StatusOK 는 baseline 과 impact 둘 다 유효 표본을 가지고 정상 산출이 성공한 상태다.
	StatusOK Status = "ok"
	// StatusSkippedLowBaseline 은 baseline 평균이 minBaselineThreshold 미만이라 division by zero
	// 또는 산출 안정성 부족으로 skip 된 상태다. 트래픽이 거의 없는 victim 이 흔히 본 status 로 분류
	// 된다.
	StatusSkippedLowBaseline Status = "skipped_low_baseline"
	// StatusSkippedNoSamples 는 NaN / Inf pairwise 제거 후 baseline 또는 impact 표본이 0 인 상태다.
	StatusSkippedNoSamples Status = "skipped_no_samples"
)

// minBaselineThreshold 는 baseline 평균이 본 값 미만이면 산출을 skip 한다. 100µs 는 nginx 같은
// 극단적으로 가벼운 서버의 idle latency 보다도 작은 수준이라 본 임계 미만이면 측정 자체의 신뢰성이
// 낮다고 본다.
const minBaselineThreshold = 100e-6

// Compute 는 baseline 과 impact 두 시계열의 평균을 비교해 0 ~ 1 정규화 score 를 산출한다. score
// 정의는 다음과 같다.
//
//	score = clamp((mean(impact) - mean(baseline)) / mean(baseline), 0, 1)
//
// impact 가 baseline 보다 작거나 같으면 score 0 으로 떨어진다 (improvement 는 noisy neighbor 모델
// 에 부합하지 않음). impact 가 baseline 의 2 배 이상이면 score 1 로 clamp 된다. 본 정의는 latency
// 처럼 "낮을수록 좋고 늘면 손해" 인 메트릭 한정이다.
//
// 가드:
//   - 입력 슬라이스의 NaN / Inf 는 모두 제거 후 평균 산출
//   - 제거 후 표본 수가 0 인 입력은 StatusSkippedNoSamples 반환
//   - baseline 평균이 minBaselineThreshold 미만이면 StatusSkippedLowBaseline 반환 (score=0)
//   - 정상 산출은 StatusOK 와 함께 0 ~ 1 사이 score 반환
func Compute(baseline, impact []float64) (float64, Status) {
	baseSamples := filterFinite(baseline)
	impactSamples := filterFinite(impact)
	if len(baseSamples) == 0 || len(impactSamples) == 0 {
		return 0, StatusSkippedNoSamples
	}
	baseMean := mean(baseSamples)
	if baseMean < minBaselineThreshold {
		return 0, StatusSkippedLowBaseline
	}
	impactMean := mean(impactSamples)
	delta := (impactMean - baseMean) / baseMean
	if delta < 0 {
		delta = 0
	}
	if delta > 1 {
		delta = 1
	}
	return delta, StatusOK
}

// VictimCandidate 는 victim 후보 1 건을 식별하는 최소 라벨 셋이다. injector 가 Prometheus 에서
// fetch 한 시계열을 본 struct 로 변환한 후 VictimCandidates 로 필터한다.
type VictimCandidate struct {
	Namespace string
	Pod       string
	PodUID    string
	Node      string
}

// VictimCandidates 는 cluster 전체 후보 중 target Pod 와 같은 노드의 다른 Pod 만 victim 으로 추린다.
// 같은 노드 조건은 cgroup 격리 환경에서 자원 경쟁이 일어날 수 있는 최소 단위라 본 시리즈의 다른
// 분석 layer (correlation) 와 동일한 가정을 따른다. self exclusion 으로 target 자신은 제외한다.
//
// 결과는 deterministic 한 순서로 정렬되어 (Namespace, Pod, PodUID) lexicographic 으로 emit 된다.
// MaxVictims 가 양수면 정렬 후 상위 N 개만 채택한다.
func VictimCandidates(all []VictimCandidate, targetNode, targetNamespace, targetPod string, maxVictims int) []VictimCandidate {
	out := make([]VictimCandidate, 0, len(all))
	for _, c := range all {
		if c.Node != targetNode {
			continue
		}
		if c.Namespace == targetNamespace && c.Pod == targetPod {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		if out[i].Pod != out[j].Pod {
			return out[i].Pod < out[j].Pod
		}
		return out[i].PodUID < out[j].PodUID
	})
	if maxVictims > 0 && len(out) > maxVictims {
		out = out[:maxVictims]
	}
	return out
}

func filterFinite(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, v := range in {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, v)
	}
	return out
}

func mean(in []float64) float64 {
	if len(in) == 0 {
		return 0
	}
	var sum float64
	for _, v := range in {
		sum += v
	}
	return sum / float64(len(in))
}
