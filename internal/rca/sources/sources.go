// Package sources 는 registry 패키지의 Sources 인터페이스를 구현하는 외부 자료 fetcher 들을 모은다.
// snapshot.go 는 correlation-exporter 의 /snapshot 응답을 HTTP fetch + in-memory cache 로 활용하고,
// promql.go 는 Prometheus instant query 로 drop flow Top-N 을 한 번 호출한다. 두 source 모두
// webhook 응답 30 초 임계를 통과하도록 짧은 timeout + stale-OK fallback 패턴을 사용한다.
package sources

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"netobs/internal/rca/registry"
)

// DefaultSnapshotTTL 은 correlation snapshot in-memory cache 의 유효 기간이다. correlation-exporter
// reconcile 주기가 5 분이라 cache 가 5 분보다 길면 stale 신호를 노출하게 되고 짧으면 매 webhook
// 마다 HTTP fetch 가 발생한다. 본 값은 두 케이스의 중간 (90 초) 으로 두어 운영 부담과 신선도를
// 균형 잡는다.
const DefaultSnapshotTTL = 90 * time.Second

// DefaultFetchTimeout 은 단일 외부 호출 (snapshot HTTP, Prometheus query) 의 wall-clock 상한이다.
// webhook 임계 30 초의 1/6 으로 두어 두 source 가 직렬 호출되어도 18 초 안에 끝난다.
const DefaultFetchTimeout = 5 * time.Second

// DefaultTopN 은 Sources 가 돌려주는 최대 항목 수다. registry 가 [0] 만 참조하므로 5 면 충분하다.
const DefaultTopN = 5

// Sources 는 snapshotSource 와 promQLSource 와 gpuobsSource 세 갈래를 하나의 registry.Sources
// 인터페이스 구현 으로 묶는 어댑터다. 어느 한쪽이 unreachable / timeout 이어도 다른 쪽 결과 가
// mapping 에 그대로 전달 되어 RCA 산정이 완전히 끊기지 않는다.
type Sources struct {
	snapshot snapshotSource
	promql   promQLSource
	gpuobs   gpuobsSource
	topN     int
}

// gpuobsSource 는 GPU 신호 fetch 의 추상 인터페이스다. test 가 in-memory fake 를 주입할 수 있게
// 한다.
type gpuobsSource interface {
	fetchGPUSignal(node string) float64
}

// New 는 production Sources 를 만든다. snapshotURL 은 correlation-exporter 의 /snapshot URL,
// prometheusURL 은 Prometheus base URL 이다. fetchTimeout / snapshotTTL / topN 은 zero 값일 때
// 본 패키지 default 가 적용된다.
func New(snapshotURL, prometheusURL string, fetchTimeout, snapshotTTL time.Duration, topN int) *Sources {
	if fetchTimeout <= 0 {
		fetchTimeout = DefaultFetchTimeout
	}
	if snapshotTTL <= 0 {
		snapshotTTL = DefaultSnapshotTTL
	}
	if topN <= 0 {
		topN = DefaultTopN
	}
	return &Sources{
		snapshot: newHTTPSnapshotSource(snapshotURL, fetchTimeout, snapshotTTL),
		promql:   newHTTPPromQLSource(prometheusURL, fetchTimeout),
		gpuobs:   newHTTPGpuobsSource(prometheusURL, fetchTimeout),
		topN:     topN,
	}
}

// GPUSignal 은 #122 의 multi-source cross-reference 산출 시 GPU 도메인 신호 강도 (0-1) 를
// 돌려준다. gpuobsSource 의 Prometheus instant query 가 timeout 또는 빈 결과 면 0 을 돌려주어
// confidence 가 자연 감쇠 된다. 테스트 또는 부분 초기화 환경 에서 gpuobs 가 nil 인 경우 panic
// 회피 위해 0 을 돌려준다. 본 가드 는 다른 두 source (snapshot 과 promql) 의 빈 결과 처리 와
// 동일 한 graceful empty 계약 을 유지 한다.
func (s *Sources) GPUSignal(node string) float64 {
	if s.gpuobs == nil {
		return 0
	}
	return s.gpuobs.fetchGPUSignal(node)
}

// Probe 는 readiness 용 초기 connectivity 검사다. correlation-exporter snapshot 또는 Prometheus
// query 중 하나라도 연결 되면 nil 을 돌려준다. 둘 다 실패 하면 두 에러 를 합쳐 돌려준다. rca-summarizer
// main 이 본 결과 로 readyz 를 게이팅 하며, 본 검사 와 무관 하게 webhook 수신 은 계속 serve 된다.
func (s *Sources) Probe(ctx context.Context) error {
	errSnap := fmt.Errorf("snapshot source not configured")
	if s.snapshot != nil {
		if errSnap = s.snapshot.probe(ctx); errSnap == nil {
			return nil
		}
	}
	errQuery := fmt.Errorf("promql source not configured")
	if s.promql != nil {
		if errQuery = s.promql.probe(ctx); errQuery == nil {
			return nil
		}
	}
	return fmt.Errorf("sources probe failed: snapshot=%v, query=%v", errSnap, errQuery)
}

// EvaluateConfidence 는 mapping 이 각 source 의 raw 결과 를 모은 뒤 호출 하는 multi-source
// confidence score 산출 진입점 이다. 가중치 정책 (correlation 0.5 와 netobs 0.3 과 gpuobs
// 0.2) 과 정규화 식 은 ComputeConfidenceScore 가 single source of truth 로 보유 한다. 본
// 메서드 는 source 별 raw 결과 를 ConfidenceFactors 로 변환 한 뒤 ComputeConfidenceScore 에
// 위임 한다.
func (s *Sources) EvaluateConfidence(neighbors []registry.NeighborInfo, dropFlows []registry.DropFlowInfo, gpuSignal float64) float64 {
	return ComputeConfidenceScore(ConfidenceFactors{
		Correlation: maxNeighborScore(neighbors),
		Netobs:      maxDropFlowFactor(dropFlows),
		Gpuobs:      gpuSignal,
	})
}

// TopNeighbors 는 snapshotSource 에서 victim 매칭 entry 를 모두 모은 뒤 Score 절대값 내림차순
// 으로 정렬해 상위 topN 을 돌려준다. correlation-exporter snapshot 은 (victim, dimension, rank)
// 그룹 단위 정렬이라 victim 매칭 entry 가 등장 순서로는 가장 강한 score 가 [0] 이라는 보장이 없다.
// dispatch 측 (registry idle / gpuobs mapping) 은 [0] 만 참조하므로 본 자리에서 정렬을 강제해야
// 진짜 dominant suspect 가 채택된다. snapshot fetch / parse 실패 시 빈 슬라이스를 돌려주어
// mapping 이 fallback 경로로 진입한다.
func (s *Sources) TopNeighbors(victimNamespace, victimPod string) []registry.NeighborInfo {
	snap := s.snapshot.fetch()
	candidates := make([]registry.NeighborInfo, 0, len(snap))
	for _, n := range snap {
		if n.VictimNamespace != victimNamespace || n.VictimPod != victimPod {
			continue
		}
		if n.SuspectNamespace == victimNamespace && n.SuspectPod == victimPod {
			continue // self-match 회피
		}
		candidates = append(candidates, registry.NeighborInfo{
			SuspectNamespace: n.SuspectNamespace,
			SuspectPod:       n.SuspectPod,
			Dimension:        n.Dimension,
			Score:            n.Score,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return absScore(candidates[i].Score) > absScore(candidates[j].Score)
	})
	if len(candidates) > s.topN {
		candidates = candidates[:s.topN]
	}
	return candidates
}

// absScore 는 Pearson 상관계수의 부호와 무관하게 상관 강도만 비교하기 위한 헬퍼다. NoisyNeighbor
// 의 Score 는 max_abs_value 라 이미 비음수지만 향후 signed score 가 도입되어도 본 자리의 정렬
// 의미가 깨지지 않게 한다.
func absScore(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// TopDropFlows 는 promQLSource 에 namespace 필터 instant query 를 위임한다. fetch 실패 시 빈
// 슬라이스를 돌려준다.
func (s *Sources) TopDropFlows(namespace string) []registry.DropFlowInfo {
	flows := s.promql.fetchTopDropFlows(namespace, s.topN)
	return flows
}

// snapshotEntry 는 correlation-exporter 의 /snapshot JSON 응답 element 의 부분 view 다. 본
// 패키지가 필요로 하는 필드만 추려 의존을 가볍게 한다. 라벨 이름은 correlation.NoisyNeighbor 의
// JSON tag (victim, suspect 의 nested PodIdentity) 와 정확히 일치하도록 unmarshal 단계에서
// flatten 처리한다 (snapshot.go 의 unmarshal 함수 참조).
type snapshotEntry struct {
	VictimNamespace  string  `json:"-"`
	VictimPod        string  `json:"-"`
	SuspectNamespace string  `json:"-"`
	SuspectPod       string  `json:"-"`
	Dimension        string  `json:"-"`
	Score            float64 `json:"-"`
}

// snapshotSource 는 snapshot fetch 의 추상 인터페이스다. test 가 in-memory fake 를 주입할 수
// 있게 한다. probe 는 readiness 용 connectivity 검사로 cache 를 우회 해 연결 성공 여부만 돌려준다.
type snapshotSource interface {
	fetch() []snapshotEntry
	probe(ctx context.Context) error
}

// promQLSource 는 Prometheus query 의 추상 인터페이스다.
type promQLSource interface {
	fetchTopDropFlows(namespace string, n int) []registry.DropFlowInfo
	probe(ctx context.Context) error
}

// noopSnapshot 과 noopPromQL 은 unit test 의 baseline fixture 다. registry 단위 테스트는 fake
// Sources 를 직접 만들어 쓰므로 본 noop 은 sources 패키지 내 통합 테스트에서만 사용한다. probe 는
// 항상 연결 성공 (nil) 을 돌려준다.
type noopSnapshot struct{}

func (noopSnapshot) fetch() []snapshotEntry      { return nil }
func (noopSnapshot) probe(context.Context) error { return nil }

type noopPromQL struct{}

func (noopPromQL) fetchTopDropFlows(string, int) []registry.DropFlowInfo { return nil }
func (noopPromQL) probe(context.Context) error                           { return nil }

type noopGpuobs struct{}

func (noopGpuobs) fetchGPUSignal(string) float64 { return 0 }

// staleCache 는 snapshot 의 TTL 기반 in-memory cache 다. fetch 시 cache hit 이면 mu 잠금만으로
// stale-OK 반환, miss 면 caller 가 backing fetcher 호출 후 store 한다.
type staleCache struct {
	mu        sync.RWMutex
	entries   []snapshotEntry
	updatedAt time.Time
	ttl       time.Duration
}

func newStaleCache(ttl time.Duration) *staleCache {
	return &staleCache{ttl: ttl}
}

func (c *staleCache) get() ([]snapshotEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.entries == nil {
		return nil, false
	}
	if time.Since(c.updatedAt) > c.ttl {
		return c.entries, false // stale but returnable
	}
	return c.entries, true
}

func (c *staleCache) store(entries []snapshotEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = entries
	c.updatedAt = time.Now()
}
