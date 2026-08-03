// Package exporter 는 internal/correlation 의 산출 결과를 Prometheus 메트릭으로 노출한다.
// correlation-exporter 바이너리가 본 패키지의 Collector 와 Health 를 wire-up 하고 reconcile 루프로
// snapshot 을 주기적 교체한다.
package exporter

import (
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"netobs/internal/correlation"
)

// neighborLabels 는 correlation_noisy_neighbor_* 메트릭에 공통으로 부여되는 8개 라벨이다. victim 과
// suspect 양쪽의 (namespace, pod, pod_uid) 6 개에 resource_dimension 과 rank 가 추가된다. rank 는
// SelectTopN 의 TopN 가드로 1..N 범위라 카디널리티가 통제된다.
var neighborLabels = []string{
	"victim_namespace",
	"victim_pod",
	"victim_pod_uid",
	// #150 victim_signal 은 영향 종착 차원 (latency / throughput / error) 이다. resource_dimension
	// (suspect 자원 종류) 과 직교하며 한 victim 이 신호별로 독립 시리즈를 갖는다.
	"victim_signal",
	"suspect_namespace",
	"suspect_pod",
	"suspect_pod_uid",
	"resource_dimension",
	"rank",
}

// crossNodeLabels 는 #84 의 correlation_cross_node_score 메트릭 라벨 셋 이다. pod 정보 없 이 노드
// 한 쌍 과 dimension 만 으로 cardinality 가 노드 수^2 * 4 로 cap 된다. 기존 neighborLabels 와 의 의
// 도 적인 분리 로 pod-level 과 node-level 두 view 가 alert / dashboard 에서 독립 적 으로 다뤄진다.
var crossNodeLabels = []string{
	"victim_node",
	"suspect_node",
	"dimension",
}

// serviceImpactLabels 는 #148 의 correlation_service_impact_score 메트릭 라벨 셋이다. victim 을 K8s
// Service 에 근사하는 workload (namespace, workload) 와 suspect_node 와 dimension 만으로 cardinality 가
// workload 수 * 노드 수 * 4 로 cap 된다. neighborLabels / crossNodeLabels 와 의도적으로 분리해 pod /
// node / workload 세 view 가 alert / dashboard 에서 독립적으로 다뤄진다.
var serviceImpactLabels = []string{
	"victim_namespace",
	"victim_workload",
	"suspect_node",
	"dimension",
}

// crossLevelLabels 는 #149 의 correlation_cross_level_score 메트릭 라벨 셋이다. 동일 node 안의 node↔pod
// 영향을 node 와 pod (namespace, pod) 와 direction (node_to_pod / pod_to_node) 과 dimension 으로 식별
// 한다. 한 (node, direction, dimension) 그룹 안에서 pod 가 달라 라벨 셋이 유일하므로 rank 는 라벨에
// 넣지 않는다. 다른 layer 의 라벨 셋과 분리해 네 view 가 독립적으로 다뤄진다.
var crossLevelLabels = []string{
	"node",
	"pod_namespace",
	"pod",
	"direction",
	"dimension",
}

// impactGraphLabels 는 #151 의 correlation_impact_graph_node_degree 메트릭 라벨 셋이다. 영향 전파
// 그래프 정점 (pod) 의 차수를 direction (out=영향을 주는 엣지 수, in=영향을 받는 엣지 수) 으로 구분
// 노출한다. out 이 크고 in 이 0 인 pod 는 근원 suspect 후보다. 정점 수가 noisy neighbor Top-N 으로
// 통제되어 cardinality 가 작다.
var impactGraphLabels = []string{
	"namespace",
	"pod",
	"pod_uid",
	"direction",
}

// impactRootReachLabels 는 #151 Phase 2 의 correlation_impact_root_reach 메트릭 라벨 셋이다. 근원
// suspect (in-degree 0 정점) 의 (namespace, pod, pod_uid) 로 식별하며 root 수가 적어 cardinality 가
// 작다.
var impactRootReachLabels = []string{
	"namespace",
	"pod",
	"pod_uid",
}

// Collector 는 마지막 reconcile cycle 의 NoisyNeighbor snapshot 을 보관해 Prometheus scrape 시점에
// score 와 lag 메트릭으로 emit 한다. prometheus.Collector 인터페이스를 직접 구현해 snapshot 교체
// 시 GaugeVec.Reset() 패턴이 가질 race 위험을 차단하고 stale series 가 코드 경로상 존재하지 않게
// 한다.
type Collector struct {
	mu       sync.RWMutex
	snapshot []correlation.NoisyNeighbor
	// dominant 는 Replace 시점에 ComputeDominantDimension 결과를 미리 산정해 둔 캐시다. Prometheus
	// scrape 가 호출되는 Collect hot path 에서 매번 victim 단위 dimension max 집계와 sum 정규화를
	// 재실행하지 않게 한다. snapshot 이 바뀔 때만 갱신된다.
	dominant []correlation.DominantDimension
	// crossNode 는 #84 의 cross-node interference snapshot 이다. ReplaceCrossNode 가 reconcile cycle
	// 마다 갱신 하고 Collect 가 correlation_cross_node_score gauge 로 emit 한다. nil 또는 빈 슬라이스
	// 면 series 가 0 개 emit 되어 cross-node opt-in 비활성 운영 모드 와 정합 한다.
	crossNode []correlation.NodeInterference
	// serviceImpact 는 #148 의 service-impact snapshot 이다. ReplaceServiceImpact 가 reconcile cycle
	// 마다 갱신하고 Collect 가 correlation_service_impact_score gauge 로 emit 한다. nil 또는 빈 슬라이스
	// 면 series 가 0 개 emit 되어 service-impact opt-out 운영 모드와 정합한다.
	serviceImpact []correlation.ServiceImpact
	// crossLevel 는 #149 의 cross-level snapshot 이다. ReplaceCrossLevel 가 reconcile cycle 마다 갱신
	// 하고 Collect 가 correlation_cross_level_score gauge 로 emit 한다. nil 또는 빈 슬라이스면 series 가
	// 0 개 emit 되어 cross-level opt-out 운영 모드와 정합한다.
	crossLevel []correlation.CrossLevel
	// impactGraph 는 #151 Phase 1 의 영향 전파 그래프 snapshot 이다. ReplaceImpactGraph 가 reconcile
	// cycle 마다 갱신하고 Collect 가 correlation_impact_graph_node_degree gauge 로, API 가 nodes / edges
	// 로 노출한다. 빈 그래프면 series 가 0 개 emit 되어 ImpactGraphEnabled=false opt-out 과 정합한다.
	impactGraph correlation.ImpactGraph
	// impactPaths 와 rootSuspects 는 #151 Phase 2 의 다단계 경로와 근원 suspect 집계다. ReplaceImpactPaths
	// 가 reconcile cycle 마다 paths 를 받아 RootSuspects 로 root 를 함께 산정해 캐시하고, Collect 가
	// correlation_impact_root_reach gauge 로, API 가 paths / roots 로 노출한다.
	impactPaths  []correlation.ImpactPath
	rootSuspects []correlation.RootSuspect
	// step 은 LagSteps 를 초 단위로 변환할 때 곱해진다. exporter 가 Correlator 의 Config.Step 과
	// 동일 값을 받아 lag step 의 시간 의미를 보존한다.
	step time.Duration

	scoreDesc              *prometheus.Desc
	lagDesc                *prometheus.Desc
	pvalueDesc             *prometheus.Desc
	impactDesc             *prometheus.Desc
	impactMagnitudeDesc    *prometheus.Desc
	impactPValueDesc       *prometheus.Desc
	causalStrengthDesc     *prometheus.Desc
	dominantDesc           *prometheus.Desc
	crossNodeScoreDesc     *prometheus.Desc
	serviceImpactScoreDesc *prometheus.Desc
	crossLevelScoreDesc    *prometheus.Desc
	impactGraphDegreeDesc  *prometheus.Desc
	impactRootReachDesc    *prometheus.Desc
}

// NewCollector 는 Prometheus scrape 시 emit 할 metric desc 두 개를 미리 만들어 두는 Collector 를
// 구성한다. step 은 lag step 의 시간 의미를 보존하기 위해 reconcile config 의 Step 과 동일 값을
// 전달한다.
func NewCollector(step time.Duration) *Collector {
	return &Collector{
		step: step,
		scoreDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_score",
			"Pearson 상관계수 최대 절대값. 1.0 에 가까울수록 suspect 자원 압박과 victim latency 가 강한 동조를 보인다.",
			neighborLabels, nil,
		),
		lagDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_lag_seconds",
			"score 가 최대 절대값을 보인 lag 의 초 단위 환산. 양수면 suspect 변동이 victim latency 를 N 초 선행하는 인과 방향이다.",
			neighborLabels, nil,
		),
		pvalueDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_pvalue",
			"#69 의 Granger causality p-value. src (suspect) 가 dst (victim latency) 를 Granger-cause 하는지의 통계적 유의성. 0.05 미만이면 high-confidence 인과 신호로 본다. continuous 값이라 라벨이 아닌 별개 메트릭으로 분리해 cardinality 폭증을 차단한다.",
			neighborLabels, nil,
		),
		impactDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_impact_seconds",
			"#146 의 effect size (latency victim 전용 legacy). suspect 압박 구간과 비압박 구간의 victim latency 차이 (seconds) 로 간섭의 절대 영향 크기다. #175 부터 throughput / error / gpu victim 까지 확장된 native 단위 크기는 correlation_noisy_neighbor_impact_magnitude 를, 그 차이의 통계적 유의성은 correlation_noisy_neighbor_impact_pvalue 를 본다. 표본 부족 등으로 산정이 skip 된 시리즈는 emit 되지 않아 0 noise 가 끼지 않는다.",
			neighborLabels, nil,
		),
		impactMagnitudeDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_impact_magnitude",
			"#175 의 effect size 크기. suspect 압박 구간과 비압박 구간의 victim 값 차이를 victim 신호별 native 단위로 노출한다 (victim_signal=latency 면 seconds 증가, throughput 이면 bytes/s 감소, error 면 drops/s 증가, gpu 면 util 감소). 단위가 신호별로 다르므로 victim_signal 라벨과 함께 해석한다. degradation 이 없거나 표본 부족이면 emit 되지 않는다.",
			neighborLabels, nil,
		),
		impactPValueDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_impact_pvalue",
			"#175 의 effect size 유의성. high (압박) / low (비압박) 두 구간 victim 평균 차이의 Welch two-sample t-test two-sided p-value 다. 0.05 미만이면 압박에 따른 victim 품질 차이가 우연이 아닌 유의한 간섭 신호로 본다. 표본 부족이나 구간 분산이 사실상 0 이면 산정이 graceful skip 되어 emit 되지 않는다.",
			neighborLabels, nil,
		),
		causalStrengthDesc: prometheus.NewDesc(
			"correlation_noisy_neighbor_causal_strength",
			"#176 의 통합 인과강도. Pearson 강도 (0.5) 와 Granger 유의성 (0.3) 과 effect size 유의성 (0.2) 을 가중합한 0~1 단일 지표로, 운영자가 개별 필드 (score / pvalue / impact_pvalue) 를 종합하지 않고 한 값으로 간섭 여부를 판단한다. 유의성 항은 산정 skip 시 0 으로 처리되어 증거가 많을수록 1 에 가깝다. 산정식과 가중치는 ComputeCausalStrength 가 단일 진실원으로 보유한다.",
			neighborLabels, nil,
		),
		dominantDesc: prometheus.NewDesc(
			"correlation_dominant_dimension",
			"#69 의 victim 단위 dominant dimension. 4 dimension (cpu / gpu / memory / network) 별 max score 를 sum 정규화한 weight 중 가장 큰 dimension 1 종만 emit 된다. 정확 동률 시 dimension enum 사전순 가장 앞 라벨이 채택된다. raw 메트릭이라 latency pressure 와 무관하게 항상 emit 되며 active 시간대 한정 view 는 correlation_dominant_dimension_active:5m recording rule 을 본다.",
			[]string{"victim_namespace", "victim_pod", "victim_pod_uid", "dimension"}, nil,
		),
		crossNodeScoreDesc: prometheus.NewDesc(
			"correlation_cross_node_score",
			"#84 cross-node interference layer 의 Pearson 상관계수 최대 절대값. suspect_node 의 자원 압박 (dimension) 과 victim_node 의 p99 latency 사이의 동조 정도다. CrossNodeEnabled opt-in 시 만 emit 되며 victim_node == suspect_node 인 시리즈는 enumerate 단에서 자동 제외된다.",
			crossNodeLabels, nil,
		),
		serviceImpactScoreDesc: prometheus.NewDesc(
			"correlation_service_impact_score",
			"#148 service-impact layer 의 Pearson 상관계수 최대 절대값. suspect_node 의 자원 압박 (dimension) 과 victim workload (K8s Service 근사, namespace/workload) 의 p99 latency 사이의 동조 정도다. victim 은 netobs 의 src_workload 라벨로 집계되며 ServiceImpactEnabled opt-in 시 만 emit 된다.",
			serviceImpactLabels, nil,
		),
		crossLevelScoreDesc: prometheus.NewDesc(
			"correlation_cross_level_score",
			"#149 cross-level layer 의 Pearson 상관계수 최대 절대값. 동일 node 안에서 node 압박과 pod latency 사이의 동조 정도다. direction=node_to_pod 면 node 압박이 pod latency 에 주는 영향, pod_to_node 면 pod 압박이 node latency 에 주는 영향이며 dimension 은 압박 쪽에서 분류된다. CrossLevelEnabled opt-in 시 만 emit 된다.",
			crossLevelLabels, nil,
		),
		impactGraphDegreeDesc: prometheus.NewDesc(
			"correlation_impact_graph_node_degree",
			"#151 Phase 1 영향 전파 그래프 정점의 차수. direction=out 은 이 pod 가 suspect 인 엣지 수 (영향을 주는 관계), in 은 victim 인 엣지 수 (영향을 받는 관계) 다. out 이 크고 in 이 작은 pod 는 다단계 전파의 근원 suspect 후보, in 이 큰 pod 는 영향이 모이는 victim hub 다. ImpactGraphEnabled opt-in 시 만 emit 된다.",
			impactGraphLabels, nil,
		),
		impactRootReachDesc: prometheus.NewDesc(
			"correlation_impact_root_reach",
			"#151 Phase 2 근원 suspect (in-degree 0 정점) 의 영향 범위. 본 root 에서 다단계 전파 경로로 도달 가능한 distinct 종착 victim 수다. 값이 클수록 가장 넓게 영향을 퍼뜨리는 근본 원인 후보다. ImpactGraphEnabled opt-in 시 경로 추출과 함께 emit 된다.",
			impactRootReachLabels, nil,
		),
	}
}

// Snapshot 은 가장 최근 Replace 가 보관한 NoisyNeighbor 리스트의 안전한 복사본을 반환한다.
// rca-summarizer 가 /snapshot endpoint 로 본 결과를 in-memory cache hit 으로 활용해 webhook
// 응답 시점에 매번 Prometheus query 로 Top-N 을 재계산하지 않게 한다. 반환 슬라이스는 호출자가
// 자유롭게 수정해도 내부 상태에 영향이 없다.
func (c *Collector) Snapshot() []correlation.NoisyNeighbor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.snapshot) == 0 {
		return nil
	}
	return append([]correlation.NoisyNeighbor(nil), c.snapshot...)
}

// Replace 는 reconcile cycle 산출물로 snapshot 을 교체한다. 입력 슬라이스는 호출 후에도 호출자
// 측에서 수정 가능하도록 내부적으로 복사본을 보관한다. dominant dimension 도 본 시점에 1 회
// 산정해 캐시에 둬 scrape 마다 Collect 가 재계산하지 않게 한다.
func (c *Collector) Replace(neighbors []correlation.NoisyNeighbor) {
	copied := append([]correlation.NoisyNeighbor(nil), neighbors...)
	dominant := correlation.ComputeDominantDimension(copied)
	c.mu.Lock()
	c.snapshot = copied
	c.dominant = dominant
	c.mu.Unlock()
}

// ReplaceCrossNode 는 #84 cross-node interference의 snapshot을 교체한다. main 의 reconcileOnce 가
// CrossNodeEnabled 와 무관하게 매 cycle 본 함수를 호출하나, 비활성 운영 모드에서는 SelectTopN
// CrossNode 가 IsCrossNode=true 항목 0 개 결과 (빈 슬라이스) 를 돌려 주므로 crossNode 가 비어 있어
// series 가 emit 되지 않는다.
func (c *Collector) ReplaceCrossNode(crossNode []correlation.NodeInterference) {
	copied := append([]correlation.NodeInterference(nil), crossNode...)
	c.mu.Lock()
	c.crossNode = copied
	c.mu.Unlock()
}

// CrossNodeSnapshot 은 가장 최근 ReplaceCrossNode 가 보관한 NodeInterference 리스트 의 안전한 복사본
// 을 반환 한다. #119 의 /api/v1/cross-node-interference endpoint 가 본 메서드 를 in-memory read 로 호출
// 해 Prometheus query 재계산 없이 cross-node snapshot 을 외부 시스템 (RCA summarizer 와 운영 자동화)
// 에 노출 한다. CrossNodeEnabled=false 또는 첫 reconcile 전이면 nil 을 돌려 주어 호출 측이 빈 응답
// 으로 graceful degradation 한다.
func (c *Collector) CrossNodeSnapshot() []correlation.NodeInterference {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.crossNode) == 0 {
		return nil
	}
	return append([]correlation.NodeInterference(nil), c.crossNode...)
}

// ReplaceServiceImpact 는 #148 service-impact 의 snapshot 을 교체한다. main 의 reconcileOnce 가
// ServiceImpactEnabled 와 무관하게 매 cycle 본 함수를 호출하나, opt-out 운영 모드에서는 SelectTopN
// ServiceImpact 가 IsServiceImpact=true 항목 0 개 결과 (빈 슬라이스) 를 돌려 주므로 serviceImpact 가
// 비어 있어 series 가 emit 되지 않는다.
func (c *Collector) ReplaceServiceImpact(serviceImpact []correlation.ServiceImpact) {
	copied := append([]correlation.ServiceImpact(nil), serviceImpact...)
	c.mu.Lock()
	c.serviceImpact = copied
	c.mu.Unlock()
}

// ServiceImpactSnapshot 은 가장 최근 ReplaceServiceImpact 가 보관한 ServiceImpact 리스트의 안전한
// 복사본을 반환한다. #148 의 /api/v1/service-impact endpoint 가 본 메서드를 in-memory read 로 호출해
// Prometheus query 재계산 없이 service-impact snapshot 을 외부 시스템에 노출한다. ServiceImpactEnabled=
// false 또는 첫 reconcile 전이면 nil 을 돌려 주어 호출 측이 빈 응답으로 graceful degradation 한다.
func (c *Collector) ServiceImpactSnapshot() []correlation.ServiceImpact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.serviceImpact) == 0 {
		return nil
	}
	return append([]correlation.ServiceImpact(nil), c.serviceImpact...)
}

// ReplaceCrossLevel 는 #149 cross-level 의 snapshot 을 교체한다. main 의 reconcileOnce 가 CrossLevelEnabled
// 와 무관하게 매 cycle 본 함수를 호출하나, opt-out 운영 모드에서는 SelectTopNCrossLevel 가 IsCrossLevel=
// true 항목 0 개 결과 (빈 슬라이스) 를 돌려 주므로 crossLevel 이 비어 있어 series 가 emit 되지 않는다.
func (c *Collector) ReplaceCrossLevel(crossLevel []correlation.CrossLevel) {
	copied := append([]correlation.CrossLevel(nil), crossLevel...)
	c.mu.Lock()
	c.crossLevel = copied
	c.mu.Unlock()
}

// CrossLevelSnapshot 은 가장 최근 ReplaceCrossLevel 가 보관한 CrossLevel 리스트의 안전한 복사본을
// 반환한다. #149 의 /api/v1/cross-level endpoint 가 본 메서드를 in-memory read 로 호출해 Prometheus
// query 재계산 없이 cross-level snapshot 을 외부 시스템에 노출한다. CrossLevelEnabled=false 또는 첫
// reconcile 전이면 nil 을 돌려 주어 호출 측이 빈 응답으로 graceful degradation 한다.
func (c *Collector) CrossLevelSnapshot() []correlation.CrossLevel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.crossLevel) == 0 {
		return nil
	}
	return append([]correlation.CrossLevel(nil), c.crossLevel...)
}

// ReplaceImpactGraph 는 #151 Phase 1 의 영향 전파 그래프 snapshot 을 교체한다. main 의 reconcileOnce 가
// ImpactGraphEnabled 와 무관하게 매 cycle 본 함수를 호출하나, opt-out 운영 모드에서는 빈 그래프가
// 전달되어 node degree series 가 emit 되지 않고 API 도 빈 그래프를 돌려 준다.
func (c *Collector) ReplaceImpactGraph(graph correlation.ImpactGraph) {
	c.mu.Lock()
	c.impactGraph = graph
	c.mu.Unlock()
}

// ImpactGraphSnapshot 은 가장 최근 ReplaceImpactGraph 가 보관한 ImpactGraph 의 안전한 복사본을 반환
// 한다. #151 의 /api/v1/impact-graph endpoint 가 본 메서드를 in-memory read 로 호출해 Prometheus query
// 재계산 없이 그래프를 외부 시스템에 노출한다. Nodes / Edges 슬라이스를 복사해 호출자가 수정해도
// 내부 상태가 보존된다.
func (c *Collector) ImpactGraphSnapshot() correlation.ImpactGraph {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return correlation.ImpactGraph{
		Nodes: append([]correlation.ImpactGraphNode(nil), c.impactGraph.Nodes...),
		Edges: append([]correlation.ImpactGraphEdge(nil), c.impactGraph.Edges...),
	}
}

// ReplaceImpactPaths 는 #151 Phase 2 의 다단계 경로 snapshot 을 교체하고 root 영향 범위를 함께
// 산정해 캐시한다. main 의 reconcileOnce 가 ImpactGraphEnabled 와 무관하게 매 cycle 본 함수를
// 호출하나, opt-out 또는 root 부재 (순환만 있는 그래프) 면 빈 슬라이스가 전달되어 root_reach series
// 가 emit 되지 않는다.
func (c *Collector) ReplaceImpactPaths(paths []correlation.ImpactPath) {
	copied := append([]correlation.ImpactPath(nil), paths...)
	roots := correlation.RootSuspects(copied)
	c.mu.Lock()
	c.impactPaths = copied
	c.rootSuspects = roots
	c.mu.Unlock()
}

// ImpactPathsSnapshot 은 가장 최근 ReplaceImpactPaths 가 보관한 다단계 경로의 안전한 복사본을
// 반환한다. #151 의 /api/v1/impact-paths endpoint 가 본 메서드를 in-memory read 로 호출한다.
func (c *Collector) ImpactPathsSnapshot() []correlation.ImpactPath {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.impactPaths) == 0 {
		return nil
	}
	return append([]correlation.ImpactPath(nil), c.impactPaths...)
}

// Describe 는 prometheus.Collector 인터페이스를 만족한다.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.scoreDesc
	ch <- c.lagDesc
	ch <- c.pvalueDesc
	ch <- c.impactDesc
	ch <- c.impactMagnitudeDesc
	ch <- c.impactPValueDesc
	ch <- c.causalStrengthDesc
	ch <- c.dominantDesc
	ch <- c.crossNodeScoreDesc
	ch <- c.serviceImpactScoreDesc
	ch <- c.crossLevelScoreDesc
	ch <- c.impactGraphDegreeDesc
	ch <- c.impactRootReachDesc
}

// Collect 는 현재 snapshot 의 모든 NoisyNeighbor 를 score / lag 두 메트릭으로 emit 한다. snapshot
// 이 nil 또는 빈 슬라이스면 series 를 0 개 emit 해 첫 reconcile 전 stale 0 값을 보내지 않는다. #84
// cross-node snapshot 도 동일 시점에 함께 emit 되며 두 layer 가 라벨 셋 분리 로 독립 시리즈 셋 을
// 만든다.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	snapshot := c.snapshot
	dominant := c.dominant
	crossNode := c.crossNode
	serviceImpact := c.serviceImpact
	crossLevel := c.crossLevel
	impactGraph := c.impactGraph
	rootSuspects := c.rootSuspects
	step := c.step
	c.mu.RUnlock()

	stepSeconds := step.Seconds()
	for _, n := range snapshot {
		rank := strconv.Itoa(n.Rank)
		labels := []string{
			n.Victim.Namespace,
			n.Victim.Pod,
			n.Victim.PodUID,
			string(n.VictimSignal),
			n.Suspect.Namespace,
			n.Suspect.Pod,
			n.Suspect.PodUID,
			string(n.Dimension),
			rank,
		}
		ch <- prometheus.MustNewConstMetric(c.scoreDesc, prometheus.GaugeValue, n.Score, labels...)
		ch <- prometheus.MustNewConstMetric(c.lagDesc, prometheus.GaugeValue, float64(n.LagSteps)*stepSeconds, labels...)
		if n.GrangerOK {
			ch <- prometheus.MustNewConstMetric(c.pvalueDesc, prometheus.GaugeValue, n.PValue, labels...)
		}
		if n.ImpactOK {
			ch <- prometheus.MustNewConstMetric(c.impactDesc, prometheus.GaugeValue, n.Impact, labels...)
		}
		// #175 native 단위 effect size 크기와 그 차이의 Welch t-test 유의성. 각 OK 가드로 산정 skip 된
		// 시리즈는 emit 되지 않아 0 noise 가 끼지 않는다.
		if n.ImpactMagnitudeOK {
			ch <- prometheus.MustNewConstMetric(c.impactMagnitudeDesc, prometheus.GaugeValue, n.ImpactMagnitude, labels...)
		}
		if n.ImpactPValueOK {
			ch <- prometheus.MustNewConstMetric(c.impactPValueDesc, prometheus.GaugeValue, n.ImpactPValue, labels...)
		}
		// #176 통합 인과강도. 항상 [0,1] 로 산정되어 Top-N 의 모든 페어가 단일 series 를 갖는다 (별도
		// OK 가드 없음). 운영자가 본 메트릭 하나로 간섭 우선순위를 정렬한다.
		ch <- prometheus.MustNewConstMetric(c.causalStrengthDesc, prometheus.GaugeValue, n.CausalStrength, labels...)
	}
	// dominant dimension 은 Replace 시점에 1 회 산정되어 c.dominant 에 캐시된 결과를 그대로 emit
	// 한다. scrape 마다 victim 단위 dimension max 집계 + sum 정규화 비용을 피해 Collect hot path 가
	// O(neighbors + dominant) 에 머문다.
	for _, d := range dominant {
		ch <- prometheus.MustNewConstMetric(
			c.dominantDesc,
			prometheus.GaugeValue,
			d.Weight,
			d.Victim.Namespace,
			d.Victim.Pod,
			d.Victim.PodUID,
			string(d.Dimension),
		)
	}
	// #84 cross-node interference. NodeInterference 슬라이스 의 각 항목 을 correlation_cross_node_
	// score gauge 로 emit 한다. SelectTopNCrossNode 단계 에서 victim_node == suspect_node 가 이미
	// 제외 되어 본 자리 에서 추가 가드 가 필요 없다.
	for _, n := range crossNode {
		ch <- prometheus.MustNewConstMetric(
			c.crossNodeScoreDesc,
			prometheus.GaugeValue,
			n.Score,
			n.VictimNode,
			n.SuspectNode,
			string(n.Dimension),
		)
	}
	// #148 service-impact. ServiceImpact 슬라이스의 각 항목을 correlation_service_impact_score gauge 로
	// emit 한다. SelectTopNServiceImpact 단계에서 victim/suspect 분기와 dimension 미분류 제외가 이미
	// 적용되어 본 자리에서 추가 가드가 필요 없다.
	for _, s := range serviceImpact {
		ch <- prometheus.MustNewConstMetric(
			c.serviceImpactScoreDesc,
			prometheus.GaugeValue,
			s.Score,
			s.VictimNamespace,
			s.VictimWorkload,
			s.SuspectNode,
			string(s.Dimension),
		)
	}
	// #149 cross-level. CrossLevel 슬라이스의 각 항목을 correlation_cross_level_score gauge 로 emit
	// 한다. SelectTopNCrossLevel 단계에서 방향 / dimension 미분류 제외와 (node, direction, dimension)
	// 그룹별 top-N 이 이미 적용되어 본 자리에서 추가 가드가 필요 없다.
	for _, cl := range crossLevel {
		ch <- prometheus.MustNewConstMetric(
			c.crossLevelScoreDesc,
			prometheus.GaugeValue,
			cl.Score,
			cl.Node,
			cl.PodNamespace,
			cl.Pod,
			string(cl.Direction),
			string(cl.Dimension),
		)
	}
	// #151 Phase 1 영향 전파 그래프. 각 정점의 out / in degree 를 correlation_impact_graph_node_degree
	// gauge 로 emit 한다. direction 라벨로 두 series 를 분리해 dashboard 가 근원 suspect (out 큰 pod) 와
	// victim hub (in 큰 pod) 를 바로 랭킹 가능하게 한다. 빈 그래프면 0 series 라 stale 값이 없다.
	for _, n := range impactGraph.Nodes {
		ch <- prometheus.MustNewConstMetric(
			c.impactGraphDegreeDesc, prometheus.GaugeValue, float64(n.OutDegree),
			n.Namespace, n.Pod, n.PodUID, "out",
		)
		ch <- prometheus.MustNewConstMetric(
			c.impactGraphDegreeDesc, prometheus.GaugeValue, float64(n.InDegree),
			n.Namespace, n.Pod, n.PodUID, "in",
		)
	}
	// #151 Phase 2 근원 suspect 의 영향 범위 (reach). root 별 도달 가능 distinct 종착 victim 수를 emit
	// 해 dashboard 가 가장 넓게 퍼뜨리는 근본 원인을 랭킹 가능하게 한다. root 가 없으면 0 series 다.
	for _, rs := range rootSuspects {
		ch <- prometheus.MustNewConstMetric(
			c.impactRootReachDesc, prometheus.GaugeValue, float64(rs.Reach),
			rs.Root.Namespace, rs.Root.Pod, rs.Root.PodUID,
		)
	}
}

// Health 는 exporter 자체의 동작 가시성을 위한 self-health 메트릭 셋이다. reconcile 루프가 매 cycle
// 결과에 따라 본 필드들을 갱신한다.
type Health struct {
	ReconcileDuration        prometheus.Gauge
	ReconcilePairs           prometheus.Counter
	ReconcileNeighbors       prometheus.Counter
	ReconcileSkipped         *prometheus.CounterVec
	ReconcilePartial         prometheus.Counter
	ReconcileMetricsExpected prometheus.Gauge
	ReconcileMetricsObserved prometheus.Gauge
	LastSuccessTimestamp     prometheus.Gauge
	ReconcileErrors          prometheus.Counter
	// FetchErrors 는 #405 의 per-query fetch 실패 카운터다. query 라벨은 PlannedQueries 의 고정
	// 문자열 (활성 layer 기준 수십 개) 이라 카디널리티가 통제된다. 종전에는 부분 실패가 전량 실패가
	// 아니면 어떤 신호도 남기지 않았다.
	FetchErrors *prometheus.CounterVec
}

// NewHealth 는 self-health 메트릭들을 생성해 reg 에 등록한 뒤 반환한다.
func NewHealth(reg prometheus.Registerer) *Health {
	h := &Health{
		ReconcileDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_duration_seconds",
			Help: "마지막 reconcile cycle 의 소요 시간 (초). fetch + Pearson 산출 + Top-N 선택 전체 walltime.",
		}),
		ReconcilePairs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_pairs_total",
			Help: "Correlator.Correlate 가 산출한 페어의 누적 합계.",
		}),
		ReconcileNeighbors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_neighbors_total",
			Help: "SelectTopN 채택 후 메트릭으로 emit 된 noisy neighbor 엔트리의 누적 합계.",
		}),
		ReconcileSkipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "correlation_reconcile_skipped_total",
			Help: "산출에서 skip 된 페어의 누적 합계. reason 라벨은 Pearson status 분류 (low_samples, constant) 다.",
		}, []string{"reason"}),
		ReconcilePartial: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_partial_total",
			Help: "일부 query 의 fetch 가 실패한 cycle 의 누적 (#405). 종전의 결과 기반 결측 판정은 allow-list 미설정 victim 처럼 정상적으로 빈 쿼리를 결측으로 세어 상시 오탐이었다. 운영자는 본 카운터가 증가하면 correlation_fetch_errors_total 의 query 라벨로 실패 지점을 특정한다.",
		}),
		ReconcileMetricsExpected: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_metrics_expected",
			Help: "마지막 reconcile cycle 이 fetch 를 시도한 활성 query 수 (PlannedQueries, allow-list 와 layer 활성화 반영).",
		}),
		ReconcileMetricsObserved: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_metrics_observed",
			Help: "마지막 reconcile cycle 에서 fetch 에 성공한 query 수 (#405). expected 와 다르면 그 차이만큼 fetch 실패가 있었던 상태다 (정상적으로 빈 쿼리는 성공으로 센다).",
		}),
		LastSuccessTimestamp: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "correlation_reconcile_last_success_timestamp_seconds",
			Help: "마지막 완전 성공 (fetch 실패 0건) reconcile 의 Unix epoch 초 (#405). CorrelationExporterStalled alert 의 입력이며, 부분 실패 cycle 은 갱신하지 않아 지속 부분 실패가 stalled 로 드러난다.",
		}),
		ReconcileErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "correlation_reconcile_errors_total",
			Help: "reconcile cycle 이 wrapped error 로 종료된 횟수의 누적 합계.",
		}),
		FetchErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "correlation_fetch_errors_total",
			Help: "reconcile cycle 의 per-query fetch 실패 누적 (#405). query 라벨은 실패한 PromQL 문자열이다. 부분 실패는 산출을 계속하되 본 카운터와 로그로 가시화되고, 실패가 있는 cycle 은 last_success_timestamp 를 갱신하지 않는다.",
		}, []string{"query"}),
	}
	reg.MustRegister(
		h.ReconcileDuration,
		h.ReconcilePairs,
		h.ReconcileNeighbors,
		h.ReconcileSkipped,
		h.ReconcilePartial,
		h.ReconcileMetricsExpected,
		h.ReconcileMetricsObserved,
		h.LastSuccessTimestamp,
		h.ReconcileErrors,
		h.FetchErrors,
	)
	return h
}

// RecordCycle 은 reconcile cycle 1 회의 결과를 self-health 메트릭에 반영한다. results 와 neighbors
// 의 길이 차이는 SelectTopN 의 필터링 (latency 페어 외 dedup, dimension 미분류, topN 컷) 으로 발생
// 하며 RecordCycle 은 결과 길이만 기록하고 필터별 분해는 하지 않는다 (운영자는 pairs_total 과
// neighbors_total 의 비로 필터링 비율을 관측한다).
//
// #405 partial 판정을 fetch 성공 기준으로 전환한다. 종전의 "결과에 등장한 distinct metric 수"
// 기반 판정은 allow-list 미설정 victim 처럼 정상적으로 빈 쿼리 (fetch 성공, 시리즈 0개 또는 페어
// 미생성) 를 결측으로 세어 매 cycle 오탐 증가로 신호 가치가 죽어 있었다. fetch 는 stats 로 직접
// 관측되므로 expected 는 시도한 활성 query 수, observed 는 fetch 성공 수, partial 은 실패 존재다.
// LastSuccessTimestamp 는 완전 성공 (실패 0건) cycle 만 갱신해, 지속 부분 실패가
// CorrelationExporterStalled 로 드러난다. 실패 query 별 카운터는 FetchErrors 가 담당한다.
func (h *Health) RecordCycle(duration time.Duration, results []correlation.CorrelationResult, neighbors []correlation.NoisyNeighbor, stats correlation.FetchStats) {
	h.ReconcileDuration.Set(duration.Seconds())
	h.ReconcilePairs.Add(float64(len(results)))
	h.ReconcileNeighbors.Add(float64(len(neighbors)))
	// WithLabelValues 는 매 호출마다 라벨 해시 lookup 을 수행하므로 페어가 수천 개에 이르는 hot
	// path 에서 results 루프 안에 두면 비용이 누적된다. low_samples 와 constant 두 reason 은 enum
	// 으로 고정이라 루프 진입 전에 한 번 lookup 해 캐시한다.
	lowSamples := h.ReconcileSkipped.WithLabelValues("low_samples")
	constant := h.ReconcileSkipped.WithLabelValues("constant")
	for _, r := range results {
		switch r.Status {
		case correlation.StatusSkippedLowSamples:
			lowSamples.Inc()
		case correlation.StatusSkippedConstant:
			constant.Inc()
		}
	}
	failed := len(stats.FailedQueries)
	for _, q := range stats.FailedQueries {
		h.FetchErrors.WithLabelValues(q).Inc()
	}
	h.ReconcileMetricsExpected.Set(float64(stats.Attempted))
	h.ReconcileMetricsObserved.Set(float64(stats.Attempted - failed))
	if failed > 0 {
		h.ReconcilePartial.Inc()
		return
	}
	h.LastSuccessTimestamp.SetToCurrentTime()
}

// RecordError 는 reconcile cycle 이 error 로 종료됐을 때 호출된다. LastSuccessTimestamp 는 갱신하지
// 않아 CorrelationExporterStalled alert 가 의도대로 발화한다.
func (h *Health) RecordError() {
	h.ReconcileErrors.Inc()
}
