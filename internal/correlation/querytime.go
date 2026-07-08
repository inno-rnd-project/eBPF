// querytime.go 는 #235 의 요청 스코프 평가 시점 전달자다. synthesis API 의 at 파라미터가 지정한
// 과거 시점을 context 값으로 실어 InstantQuerier 구현까지 전달한다. 인터페이스 시그니처를 바꾸지
// 않아 queryParallel 같은 기존 호출 경로가 ctx 전파만으로 시점 지정을 자동 지원한다.
package correlation

import (
	"context"
	"time"
)

type queryTimeKey struct{}

// WithQueryTime 은 instant query 의 평가 시점을 context 에 싣는다.
func WithQueryTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, queryTimeKey{}, t)
}

// QueryTimeFrom 은 context 에 실린 평가 시점을 꺼낸다. 미지정이면 ok=false.
func QueryTimeFrom(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(queryTimeKey{}).(time.Time)
	return t, ok
}
