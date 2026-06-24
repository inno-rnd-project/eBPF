// Package registry 는 alert 이름별 RCA 산정 함수를 모은 dispatcher 다. 11 alert mapping 을 alert
// 그룹 단위 (netobs, gpuobs, idle, correlation) 4 파일로 분리해 신규 alert 추가 시 한 파일만
// 갱신되도록 응집도를 통제한다. mapping 이 등록되지 않은 alert 는 Dispatch 가 zero RCASummary +
// false 를 돌려주어 호출 측이 raw label echo back 으로 silent drop 을 회피한다.
package registry

// RCASummary 는 한 alert 발화의 root cause analysis 요약 출력이다. JSON 직렬화되어 /rca endpoint
// 응답 본문과 /rca-summary 메트릭의 *_info 라벨 셋에 사용된다.
type RCASummary struct {
	// AlertName 은 dispatch 에 사용된 alert 이름이다. webhook payload 의 labels.alertname 과 같다.
	AlertName string `json:"alert_name"`
	// DominantDimension 은 본 alert 이 가리키는 자원 차원이다. cpu / memory / network / gpu /
	// thermal / unknown 중 하나. *_info gauge 의 라벨로도 노출된다.
	DominantDimension string `json:"dominant_dimension"`
	// TopSuspect 는 victim 의 가장 강한 neighbor 또는 alert 라벨에서 직접 추출한 의심 Pod 다.
	// "namespace/pod" 형태 string. 식별 불가 시 빈 문자열.
	TopSuspect string `json:"top_suspect"`
	// PrimaryDropFlow 는 NetObsDropBurst 같은 5-tuple 기반 alert 가 가리키는 핵심 flow 표현이다.
	// "src_pod -> dst_ip:dst_port proto reason" 형태. 해당 없는 alert 는 빈 문자열.
	PrimaryDropFlow string `json:"primary_drop_flow"`
	// EvidenceMetrics 는 운영자가 dashboard 에서 즉시 확인 가능한 메트릭 쿼리 / 라벨 hint 리스트다.
	// 본 필드는 메트릭이 아닌 JSON 응답으로만 노출된다 (cardinality 가드).
	EvidenceMetrics []string `json:"evidence_metrics"`
	// ConfidenceScore 는 #122 의 multi-source cross-reference 산출 신뢰도 점수 다. 0-1 범위 의
	// 정규화 값 이며 correlation source 와 netobs source 와 gpuobs source 의 가중치 합산 으로
	// 계산 된다. 단일 도메인 신호 만 으로는 0.5 미만 에 머물며 다중 도메인 cross-reference 가
	// 일치 할수록 1.0 에 가까워진다. webhook 의 false positive guard 가 본 점수 가 threshold
	// (기본 0.3) 미만 인 alert 의 RCA emit 을 skip 한다.
	ConfidenceScore float64 `json:"confidence_score"`
}

// Mapping 은 alert labels 를 받아 RCASummary 를 만들어 내는 단위 함수다. Sources 인터페이스를
// 받아 correlation Top-N 과 drop flow Top-N 같은 외부 자료를 활용한다.
type Mapping func(labels map[string]string, sources Sources) RCASummary

// Sources 는 mapping 이 RCA 산정에 활용하는 외부 자료 접근자다. 구체 구현은 internal/rca/sources
// 패키지가 제공하며 본 패키지는 인터페이스만 의존해 순환 import 와 테스트 mock 양쪽을 허용한다.
type Sources interface {
	// TopNeighbors 는 victim Pod 의 가장 강한 noisy neighbor N 종을 돌려준다. Sources 구현체가
	// caching 한 correlation-exporter snapshot 에서 victim 매칭 후 score 내림차순으로 잘라 준다.
	TopNeighbors(victimNamespace, victimPod string) []NeighborInfo
	// TopDropFlows 는 namespace 의 가장 빈번한 drop flow N 종을 돌려준다. 본 PR 의 registry 가
	// 사용하는 mapping 은 단일 flow 만 필요해 [0] 만 참조한다.
	TopDropFlows(namespace string) []DropFlowInfo
	// GPUSignal 은 #122 의 multi-source cross-reference 산출 시 GPU 도메인 신호 강도 (0-1) 를
	// 돌려준다. node 단위 GPU dominant cause weight 또는 GPU idle cause weight 의 Prometheus
	// instant query 결과를 활용 한다. 매칭 시리즈 가 없 거나 fetch 실패 시 0 을 돌려주어
	// confidence 산출 이 자연 감쇠 된다.
	GPUSignal(node string) float64
	// EvaluateConfidence 는 #122 의 multi-source cross-reference confidence score 산출 진입점
	// 이다. mapping 이 각 source 의 raw 결과를 모은 뒤 본 메서드를 호출 해 0-1 정규화 점수를
	// 받는다. 가중치 정책 과 정규화 식 은 sources 패키지의 ComputeConfidenceScore 가 single
	// source of truth 로 보유 한다.
	EvaluateConfidence(neighbors []NeighborInfo, dropFlows []DropFlowInfo, gpuSignal float64) float64
}

// NeighborInfo 는 Sources.TopNeighbors 가 돌려주는 단일 neighbor 의 식별자와 score 다.
type NeighborInfo struct {
	SuspectNamespace string
	SuspectPod       string
	Dimension        string
	Score            float64
}

// DropFlowInfo 는 Sources.TopDropFlows 가 돌려주는 단일 5-tuple flow 의 식별자와 rate 다.
type DropFlowInfo struct {
	SrcNamespace string
	SrcPod       string
	DstIP        string
	DstPort      string
	Protocol     string
	DropReason   string
	RatePerSec   float64
}

// Registry 는 alert 이름 → Mapping 매핑이다. 그룹별 register 함수가 각 init 단계에서 본 map 을
// 채운다.
type Registry struct {
	mappings map[string]Mapping
}

// New 는 본 패키지 의 4 그룹 mapping 을 모두 등록한 Registry 를 반환한다. 새 alert 추가는 해당
// 그룹 파일에 register 호출 한 줄을 더하면 된다.
func New() *Registry {
	r := &Registry{mappings: make(map[string]Mapping)}
	registerNetobs(r)
	registerGpuobs(r)
	registerIdle(r)
	registerCorrelation(r)
	return r
}

// Dispatch 는 alert 이름에 등록된 mapping 을 찾아 호출한다. mapping 미등록 시 (RCASummary{}, false)
// 를 돌려주며 호출 측은 raw label echo back 으로 silent drop 을 회피한다.
func (r *Registry) Dispatch(alertname string, labels map[string]string, sources Sources) (RCASummary, bool) {
	m, ok := r.mappings[alertname]
	if !ok {
		return RCASummary{AlertName: alertname}, false
	}
	summary := m(labels, sources)
	summary.AlertName = alertname
	return summary, true
}

// register 는 그룹 파일에서 호출되는 내부 헬퍼다. 중복 등록은 panic 으로 막아 alert 이름 충돌을
// 빌드 / 테스트 시점에 즉시 잡는다.
func (r *Registry) register(alertname string, m Mapping) {
	if _, exists := r.mappings[alertname]; exists {
		panic("registry: duplicate mapping for alert " + alertname)
	}
	r.mappings[alertname] = m
}

// Alertnames 는 등록된 모든 alert 이름의 정렬되지 않은 슬라이스를 돌려준다. 단위 테스트와
// dashboard 측이 mapping 셋의 정합성을 검증하는 데 사용된다.
func (r *Registry) Alertnames() []string {
	out := make([]string, 0, len(r.mappings))
	for name := range r.mappings {
		out = append(out, name)
	}
	return out
}

// labelOr 은 라벨 셋에서 키를 가져오되 없으면 fallback 을 돌려준다. mapping 들이 자주 쓰는
// 패턴이라 본 패키지 헬퍼로 둔다.
func labelOr(labels map[string]string, key, fallback string) string {
	if v, ok := labels[key]; ok && v != "" {
		return v
	}
	return fallback
}

// formatPod 는 namespace 와 pod 를 "ns/pod" 형태로 합친다. 어느 한쪽이 비어 있으면 빈 문자열을
// 돌려줘 top_suspect 라벨이 의도와 다른 값으로 emit 되지 않게 한다.
func formatPod(ns, pod string) string {
	if ns == "" || pod == "" {
		return ""
	}
	return ns + "/" + pod
}
