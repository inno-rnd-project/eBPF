package apicommon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestResponseCache_HitWithinTTL 은 TTL 안의 동일 요청이 핸들러를 재실행하지 않고 저장된 본문을
// 돌려주는지 검증한다 (#411).
func TestResponseCache_HitWithinTTL(t *testing.T) {
	var calls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	c := NewResponseCache(time.Minute, 0)
	wrapped := c.Middleware(h)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
		if rec.Body.String() != `{"ok":true}` {
			t.Fatalf("body=%q", rec.Body.String())
		}
		want := "MISS"
		if i > 0 {
			want = "HIT"
		}
		if got := rec.Header().Get("X-Cache"); got != want {
			t.Errorf("요청 %d X-Cache=%q want %q", i, got, want)
		}
	}
	if calls != 1 {
		t.Errorf("핸들러 호출=%d want 1 (TTL 안 재실행 없음)", calls)
	}
}

// TestResponseCache_KeyIncludesQuery 는 쿼리 파라미터가 다른 요청이 서로 다른 캐시 항목이 되고
// (특히 at 시점 지정), 파라미터 순서만 다른 요청은 같은 항목이 되는지 검증한다.
func TestResponseCache_KeyIncludesQuery(t *testing.T) {
	var calls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		_, _ = fmt.Fprintf(w, `{"n":%d}`, n)
	})
	c := NewResponseCache(time.Minute, 0)
	wrapped := c.Middleware(h)

	get := func(target string) string {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Body.String()
	}
	a := get("/api/v1/node/x?at=1700000000")
	b := get("/api/v1/node/x?at=1700000001")
	if a == b {
		t.Errorf("at 이 다른데 같은 캐시 항목 반환: %s", a)
	}
	if got := get("/api/v1/pods?limit=5&namespace=ns"); got != get("/api/v1/pods?namespace=ns&limit=5") {
		t.Error("파라미터 순서만 다른 요청이 다른 캐시 항목으로 처리됨")
	}
}

// TestResponseCache_ExpiresAfterTTL 은 TTL 경과 후 재실행되는지 검증한다.
func TestResponseCache_ExpiresAfterTTL(t *testing.T) {
	var calls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte("x"))
	})
	wrapped := NewResponseCache(20*time.Millisecond, 0).Middleware(h)
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	time.Sleep(40 * time.Millisecond)
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if calls != 2 {
		t.Errorf("핸들러 호출=%d want 2 (TTL 만료 후 재실행)", calls)
	}
}

// TestResponseCache_InflightSharing 은 동시 캐시 미스가 한 번만 핸들러를 타고 결과를 공유하는지
// 검증한다 (thundering herd 차단).
func TestResponseCache_InflightSharing(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-release
		_, _ = w.Write([]byte("shared"))
	})
	wrapped := NewResponseCache(time.Minute, 0).Middleware(h)

	var wg sync.WaitGroup
	bodies := make([]string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
			bodies[i] = rec.Body.String()
		}(i)
	}
	time.Sleep(30 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Errorf("핸들러 호출=%d want 1 (in-flight 병합)", calls)
	}
	for i, b := range bodies {
		if b != "shared" {
			t.Errorf("bodies[%d]=%q want shared", i, b)
		}
	}
}

// TestResponseCache_ErrorNotCached 는 비 200 응답이 저장되지 않고 원본 상태 코드로 전달되는지
// 검증한다. 장애 응답을 캐시하면 복구가 TTL 만큼 늦어진다.
func TestResponseCache_ErrorNotCached(t *testing.T) {
	var calls int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		WriteError(w, http.StatusInternalServerError, "query_failed", "boom")
	})
	wrapped := NewResponseCache(time.Minute, 0).Middleware(h)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d want 500", rec.Code)
		}
	}
	if calls != 2 {
		t.Errorf("핸들러 호출=%d want 2 (오류 응답 미캐시)", calls)
	}
}
