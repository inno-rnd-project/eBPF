package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAtParam 는 #235 시점 지정 조회를 검증한다. at (unix seconds) 이 querier 의 평가 시점으로
// 전달되고 응답 generated_at 에 반영되며, RFC3339 도 동일하게 동작한다.
func TestAtParam(t *testing.T) {
	fq := dropsFakeQuerier()
	h := NewSynthesisHandler(fq, nil, nil)
	rec := httptest.NewRecorder()
	h.GetDrops(rec, httptest.NewRequest(http.MethodGet, "/api/v1/drops?at=1783400000", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if fq.lastAt.Unix() != 1783400000 {
		t.Errorf("querier lastAt=%v want unix 1783400000 전달", fq.lastAt)
	}
	var resp DropsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := time.Unix(1783400000, 0).UTC().Format(time.RFC3339)
	if resp.GeneratedAt != want {
		t.Errorf("generated_at=%q want %q (평가 시점 반영)", resp.GeneratedAt, want)
	}

	// RFC3339 형식.
	fq2 := dropsFakeQuerier()
	h2 := NewSynthesisHandler(fq2, nil, nil)
	rec2 := httptest.NewRecorder()
	h2.GetDrops(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/drops?at=2026-07-01T00:00:00Z", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("rfc3339 status=%d want 200", rec2.Code)
	}
	if fq2.lastAt.UTC().Format(time.RFC3339) != "2026-07-01T00:00:00Z" {
		t.Errorf("querier lastAt=%v want RFC3339 시점", fq2.lastAt)
	}
}

// TestAtParam_Invalid 는 잘못된 형식이 400 으로 거부되는지 검증한다.
func TestAtParam_Invalid(t *testing.T) {
	h := NewSynthesisHandler(dropsFakeQuerier(), nil, nil)
	for _, at := range []string{"yesterday", "-5", "2026-13-99"} {
		rec := httptest.NewRecorder()
		h.GetDrops(rec, httptest.NewRequest(http.MethodGet, "/api/v1/drops?at="+at, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("at=%q status=%d want 400", at, rec.Code)
		}
	}
}

// TestAtParam_Absent 는 at 미지정 시 평가 시점이 실리지 않고 기존 동작이 유지되는지 검증한다.
func TestAtParam_Absent(t *testing.T) {
	fq := dropsFakeQuerier()
	h := NewSynthesisHandler(fq, nil, nil)
	rec := httptest.NewRecorder()
	h.GetDrops(rec, httptest.NewRequest(http.MethodGet, "/api/v1/drops", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !fq.lastAt.IsZero() {
		t.Errorf("lastAt=%v want zero (미지정 시 time 미전달)", fq.lastAt)
	}
}
