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
// 호출자는 즉시 return 한다. #411 부터 파싱된 시점은 clampAtValue 로 조회 가능 범위로 제한된다.
// 미래 시점은 현재로 정규화되고 Prometheus retention 밖 과거는 400 으로 거부되므로, 종전의 "빈 결과
// graceful 처리" 대신 명시 오류가 돌아간다 (요청 시점과 다른 데이터를 같은 응답으로 돌려주지 않기
// 위한 선택이며, 응답 캐시 키가 임의 과거 시점으로 증식하는 우회도 함께 막는다).
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
	t, cerr := clampAtValue(t)
	if cerr != nil {
		apicommon.WriteError(w, http.StatusBadRequest, "invalid_at", cerr.Error())
		return ctx, time.Time{}, false
	}
	return correlation.WithQueryTime(ctx, t), t, true
}

// atRetentionWindow 는 at 파라미터가 가리킬 수 있는 과거 범위다 (#411). Prometheus retention 이
// 40일 (deploy/monitoring 의 retention patch) 이라 그보다 과거는 어차피 빈 결과인데, 임의로 먼 과거를
// 허용하면 응답 캐시 키가 무한히 늘어나 캐시 우회 벡터가 된다. retention 보다 약간 넉넉한 45일로
// clamp 해 정상 조회에는 영향이 없게 한다.
const atRetentionWindow = 45 * 24 * time.Hour

// clampAtValue 는 at 시점을 조회 가능 범위로 제한한다 (#411). 미래 시점은 현재로 접고 (Prometheus
// 가 어차피 빈 결과를 돌려주므로 무해한 정규화), retention 밖 과거는 error 로 거부한다. 먼 과거를
// 하한으로 조용히 접으면 소비자가 요청한 시점과 다른 데이터를 같은 응답으로 받게 되므로 명시
// 거부가 정직하다. 거부는 응답 캐시 키가 임의 과거 시점으로 무한히 늘어나는 우회 벡터도 함께 막는다.
func clampAtValue(t time.Time) (time.Time, error) {
	now := time.Now().UTC()
	if t.After(now) {
		return now, nil
	}
	if floor := now.Add(-atRetentionWindow); t.Before(floor) {
		return time.Time{}, fmt.Errorf("at 은 최근 %d일 이내여야 합니다 (Prometheus retention 범위)", int(atRetentionWindow.Hours()/24))
	}
	return t, nil
}

// parseAtValue 는 RFC3339 문자열 또는 unix seconds 정수를 시각으로 해석한다. 두 분기 모두 UTC 로
// 정규화해 ctx 에 실리는 시각의 Location 이 입력 형식과 무관하게 일관되도록 한다. 본 함수는 형식만
// 해석하고 범위 판정은 하지 않는다. 미래 시점과 retention 밖 과거의 처리는 호출부 (applyAtParam) 가
// clampAtValue 로 수행한다 (#411, 미래는 현재로 정규화하고 retention 밖 과거는 거부).
func parseAtValue(raw string) (time.Time, error) {
	if sec, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if sec <= 0 {
			return time.Time{}, fmt.Errorf("unix seconds 는 양수여야 합니다: %s", raw)
		}
		return time.Unix(sec, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
