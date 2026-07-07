package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"netobs/internal/apicommon"
	"netobs/internal/correlation"
)

// applyAtParam 은 #235 의 시점 지정 조회 공용 진입점이다. at 쿼리 파라미터 (RFC3339 또는 unix
// seconds) 를 파싱해 평가 시점을 context 에 싣고, 응답 generated_at 에 쓸 시각을 돌려준다. 미지정
// 시 현재 시각과 원본 ctx 를 그대로 돌려주고, 잘못된 형식은 400 을 기록한 뒤 ok=false 를 돌려주므로
// 호출자는 즉시 return 한다. Prometheus retention 밖 시점은 쿼리가 빈 결과를 돌려줘 기존 graceful
// 경로로 자연 처리된다.
func applyAtParam(w http.ResponseWriter, r *http.Request, ctx context.Context) (context.Context, time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("at"))
	if raw == "" {
		return ctx, time.Now().UTC(), true
	}
	t, err := parseAtValue(raw)
	if err != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_at", "at 파싱 실패 (RFC3339 또는 unix seconds): "+err.Error())
		return ctx, time.Time{}, false
	}
	return correlation.WithQueryTime(ctx, t), t.UTC(), true
}

// parseAtValue 는 RFC3339 문자열 또는 unix seconds 정수를 시각으로 해석한다. 미래 시점은 Prometheus
// 가 빈 결과를 돌려주므로 별도 거부하지 않는다.
func parseAtValue(raw string) (time.Time, error) {
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if sec <= 0 {
			return time.Time{}, fmt.Errorf("unix seconds 는 양수여야 합니다: %s", raw)
		}
		return time.Unix(sec, 0), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}
