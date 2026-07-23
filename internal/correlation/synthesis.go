package correlation

import "math"

// synthesis API (health / pressure / node / events) 의 status·severity 분류 단일 진실원이다. 네
// 엔드포인트와 frontend 가 본 함수로 동일 라벨을 산정한다. health 는 0~1 로 높을수록 건강하고,
// pressure 와 zscore 는 높을수록 이상이 크다. 임계는 correlation-exporter 의 flag 로 주입 가능하게
// 두되 기본값을 본 상수로 고정한다.
const (
	// HealthOKThreshold 이상은 ok, HealthWarnThreshold 이상 OK 미만은 warn, 그 미만은 degraded.
	HealthOKThreshold   = 0.8
	HealthWarnThreshold = 0.5
	// PressureHighThreshold 이상은 high, PressureElevatedThreshold 이상 high 미만은 elevated, 그 미만은 low.
	PressureHighThreshold     = 0.7
	PressureElevatedThreshold = 0.4
	// 노드 사용량 점유율 (0~1, pod 합산 사용량 / 노드 allocatable) 등급 임계다 (#325).
	// NodeUsageDegradedThreshold 이상은 degraded, NodeUsageWarnThreshold 이상 degraded 미만은 warn.
	// limit 없는 pod 의 CPU 포화처럼 pressure 계열 (CFS throttle 기반) 에 잡히지 않는 포화를
	// node 상세 status 판정에 반영하기 위한 임계로, 분자가 5분 창 합산이라 순간 스파이크에 튀지 않는다.
	NodeUsageDegradedThreshold = 0.95
	NodeUsageWarnThreshold     = 0.85
	// ZScoreHighThreshold 이상(절대값)은 high, ZScoreElevatedThreshold 이상 high 미만은 elevated, 그 미만은 none.
	ZScoreHighThreshold     = 3.0
	ZScoreElevatedThreshold = 2.0
)

// HealthStatus 는 health score (0~1, 높을수록 건강) 를 ok / warn / degraded 로 분류한다. NaN 은 데이터
// 부재로 보아 unknown 을 돌려준다.
func HealthStatus(score float64) string {
	switch {
	case math.IsNaN(score):
		return "unknown"
	case score >= HealthOKThreshold:
		return "ok"
	case score >= HealthWarnThreshold:
		return "warn"
	default:
		return "degraded"
	}
}

// PressureSeverity 는 pressure score (0~1, 높을수록 압박) 를 low / elevated / high 로 분류한다. NaN 은
// unknown 을 돌려준다.
func PressureSeverity(score float64) string {
	switch {
	case math.IsNaN(score):
		return "unknown"
	case score >= PressureHighThreshold:
		return "high"
	case score >= PressureElevatedThreshold:
		return "elevated"
	default:
		return "low"
	}
}

// ZScoreSeverity 는 z-score 의 절대값을 none / elevated / high 로 분류한다. 부호와 무관하게 기준선 대비
// 이탈 크기만 본다. NaN 은 none 으로 둔다 (이상 없음 취급).
func ZScoreSeverity(z float64) string {
	if math.IsNaN(z) {
		return "none"
	}
	a := math.Abs(z)
	switch {
	case a >= ZScoreHighThreshold:
		return "high"
	case a >= ZScoreElevatedThreshold:
		return "elevated"
	default:
		return "none"
	}
}
