package correlation

// 노드·pod 상태 어휘의 단일 규약이다 (#381). 같은 상태 개념이 API 마다 다른 enum 으로 노출되어
// (topology 는 healthy/warning/critical/unknown, overview·node-map 은 healthy/warning/down,
// node/{node} 는 ok/warn/degraded/unknown) 프론트가 API 별 매핑 테이블을 유지해야 했다. 노드 상태는
// 아래 5값 어휘로 산정하는 것을 단일 규약으로 하고, 기존 필드는 계약 유지를 위해 각 API 의 종전 값을
// 그대로 노출한다 (node/{node} 는 NodeDetailStatus 매핑으로 환원하고 status_unified 로 규약 어휘를
// additive 노출, overview·node-map 은 critical 을 warning 으로 압축하는 3단 rollup). pod 상태는 단일
// enum 이 아니라 두 축으로 명시한다. lifecycle 축 (PodStatus*, node-map 의 status) 은 phase·alert
// 기반 생존 판정이고, severity 축 (PressureSeverity 의 low/elevated/high, node/pods·pod-detail·
// memory 의 severity) 은 pressure score 등급이다. 임계는 synthesis.go 의 상수를 그대로 쓰며 본
// 파일은 어휘 환산만 담당한다.
const (
	// NodeStatusDown 은 ready false (kube_node_status_condition 기반) 다. pressure 등급이 아니라
	// 노드 자체의 불가용이며, ready 신호를 조회하는 API (overview, node-map) 만 방출한다.
	NodeStatusDown = "down"
	// NodeStatusCritical 은 pressure high, usage degraded 임계 이상, health degraded, alert
	// severity critical 에 해당하는 최악 압박 등급이다.
	NodeStatusCritical = "critical"
	// NodeStatusWarning 은 pressure elevated, usage warn 임계 이상, health warn, firing alert
	// 존재에 해당하는 주의 등급이다.
	NodeStatusWarning = "warning"
	// NodeStatusHealthy 는 모든 판정 신호가 정상 범위인 상태다.
	NodeStatusHealthy = "healthy"
	// NodeStatusUnknown 은 판정 신호 부재 (데이터 없음) 다.
	NodeStatusUnknown = "unknown"
)

// pod lifecycle 축 어휘다 (#249 node-map pod status, #314 completed 추가). severity 축
// (low/elevated/high) 과 독립인 생존 판정으로, down 은 phase Failed/Unknown, warning 은 Pending
// 또는 firing alert 매칭, completed 는 phase Succeeded (정상 종료), live 는 그 외다.
const (
	PodStatusLive      = "live"
	PodStatusWarning   = "warning"
	PodStatusDown      = "down"
	PodStatusCompleted = "completed"
)

// NodeStatusRank 는 단일 규약 어휘의 심각도 순위다. worst-of 합성 (#324) 의 등급 비교에 쓴다.
// down 은 노드 불가용이라 pressure 계열 최악 (critical) 보다 위다.
func NodeStatusRank(s string) int {
	switch s {
	case NodeStatusDown:
		return 4
	case NodeStatusCritical:
		return 3
	case NodeStatusWarning:
		return 2
	case NodeStatusHealthy:
		return 1
	default:
		return 0
	}
}

// NodeStatusFromPressure 는 pressure score 를 단일 규약 어휘로 환산한다 (high 는 critical,
// elevated 는 warning, low 는 healthy, NaN 은 unknown). topology 의 status 와 node/{node} 의
// pressure 등급 입력이 공유한다.
func NodeStatusFromPressure(score float64) string {
	switch PressureSeverity(score) {
	case "high":
		return NodeStatusCritical
	case "elevated":
		return NodeStatusWarning
	case "low":
		return NodeStatusHealthy
	default:
		return NodeStatusUnknown
	}
}

// NodeStatusFromUsage 는 노드 점유율 (0~1, pod 합산 사용량 / allocatable, #325) 을 단일 규약
// 어휘로 환산한다. limit 없는 pod 의 포화처럼 pressure 계열에 잡히지 않는 사용량 포화 판정용이다.
func NodeStatusFromUsage(frac float64) string {
	switch {
	case frac >= NodeUsageDegradedThreshold:
		return NodeStatusCritical
	case frac >= NodeUsageWarnThreshold:
		return NodeStatusWarning
	default:
		return NodeStatusHealthy
	}
}

// NodeStatusFromHealthScore 는 health score (0~1, 높을수록 건강) 를 단일 규약 어휘로 환산한다
// (HealthStatus 의 degraded 는 critical, warn 은 warning, ok 는 healthy, unknown 은 unknown).
func NodeStatusFromHealthScore(score float64) string {
	switch HealthStatus(score) {
	case "degraded":
		return NodeStatusCritical
	case "warn":
		return NodeStatusWarning
	case "ok":
		return NodeStatusHealthy
	default:
		return NodeStatusUnknown
	}
}

// NodeDetailStatus 는 단일 규약 어휘를 node/{node} 의 기존 status 어휘 (ok/warn/degraded/unknown)
// 로 환원한다. 기존 필드 계약 유지용이며 (#381 비목표: 기존 필드 제거 없음), down 은 node/{node}
// 가 방출하지 않는 값이라 입력에 오지 않는다.
func NodeDetailStatus(unified string) string {
	switch unified {
	case NodeStatusCritical:
		return "degraded"
	case NodeStatusWarning:
		return "warn"
	case NodeStatusHealthy:
		return "ok"
	default:
		return "unknown"
	}
}
