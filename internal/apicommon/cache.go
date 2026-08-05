package apicommon

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ResponseCache 는 #411 의 짧은 TTL 응답 캐시와 in-flight 병합이다. 프론트가 10 초 주기로 폴링하는
// 엔드포인트는 5 분 recording rule 결과를 읽으므로 매 요청 재계산이 순수 낭비다. TTL 안의 동일 요청은
// 저장된 본문을 그대로 돌려주고, 캐시 미스가 동시에 겹치면 한 요청만 실제 핸들러를 타고 나머지는 그
// 결과를 공유한다 (thundering herd 차단).
//
// 캐시 키는 경로와 정렬된 쿼리 문자열 전체라 필터 파라미터가 다른 요청이 섞이지 않고, 시점 지정
// (at) 도 키에 포함되어 과거 시점 응답이 현재 시점 응답으로 오염되지 않는다. 200 응답만 저장한다
// (4xx 와 5xx 는 즉시 재시도가 정상 동작이라 캐시하면 장애 복구가 늦어진다).
type ResponseCache struct {
	ttl     time.Duration
	maxKeys int

	mu       sync.Mutex
	entries  map[string]*cacheEntry
	inflight map[string]*inflightCall
}

// cacheEntry 는 저장된 응답 1건이다. header 는 Content-Type 만 보존한다 (CORS 헤더는 요청 origin
// 마다 달라 캐시 대상이 아니며, 미들웨어 순서상 CORS 가 캐시 밖에서 매 요청 부착된다).
type cacheEntry struct {
	body        []byte
	contentType string
	expiresAt   time.Time
}

// inflightCall 은 진행 중인 캐시 미스 1건이다. 같은 키의 후속 요청은 done 을 기다린 뒤 결과를
// 공유한다.
type inflightCall struct {
	done  chan struct{}
	entry *cacheEntry
}

// NewResponseCache 는 TTL 과 키 상한으로 캐시를 만든다. ttl 이 0 이하면 캐시가 비활성 (항상 통과)
// 이다. maxKeys 초과 시 전체를 비워 무제한 증가를 막는다 (엔드포인트당 키가 필터 조합 수라 통상
// 수십 개 수준이고, 상한은 at 파라미터를 이용한 키 폭증의 backstop 이다).
func NewResponseCache(ttl time.Duration, maxKeys int) *ResponseCache {
	if maxKeys <= 0 {
		maxKeys = 512
	}
	return &ResponseCache{
		ttl:      ttl,
		maxKeys:  maxKeys,
		entries:  make(map[string]*cacheEntry),
		inflight: make(map[string]*inflightCall),
	}
}

// Middleware 는 캐시를 미들웨어로 감싼다. GET 이 아닌 요청은 그대로 통과시킨다 (MethodGuard 가
// 이미 조회 메서드만 허용하나 HEAD 는 본문이 없어 캐시 대상에서 제외한다).
func (c *ResponseCache) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c == nil || c.ttl <= 0 || r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		key := cacheKey(r)
		if e := c.lookup(key); e != nil {
			writeCached(w, e, "HIT")
			return
		}
		e, shared, raw := c.fetch(key, next, r)
		switch {
		case e != nil && shared:
			writeCached(w, e, "HIT")
		case e != nil:
			writeCached(w, e, "MISS")
		case raw != nil:
			// 200 이 아닌 응답은 캐시하지 않고 원본 그대로 전달한다 (4xx 와 5xx 는 즉시 재시도가
			// 정상 동작이라 저장하면 장애 복구가 늦어진다).
			raw.flushTo(w)
		default:
			// 선행 요청이 비 200 을 받아 공유할 결과가 없는 경우다. 자신이 직접 핸들러를 탄다.
			next.ServeHTTP(w, r)
		}
	})
}

// lookup 은 유효한 캐시 항목을 돌려준다. 만료 항목은 즉시 제거한다.
func (c *ResponseCache) lookup(key string) *cacheEntry {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil
	}
	if now.After(e.expiresAt) {
		delete(c.entries, key)
		return nil
	}
	return e
}

// fetch 는 캐시 미스를 처리한다. 같은 키의 선행 요청이 진행 중이면 그 결과를 기다려 공유하고
// (shared=true), 자신이 선행이면 핸들러를 실행해 결과를 저장한다. 200 이 아닌 응답은 저장하지 않고
// 원본 recorder 를 함께 돌려줘 호출자가 그대로 전달한다.
func (c *ResponseCache) fetch(key string, next http.Handler, r *http.Request) (*cacheEntry, bool, *cacheRecorder) {
	c.mu.Lock()
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		<-call.done
		return call.entry, true, nil
	}
	call := &inflightCall{done: make(chan struct{})}
	c.inflight[key] = call
	c.mu.Unlock()

	rec := &cacheRecorder{header: make(http.Header), status: http.StatusOK}
	next.ServeHTTP(rec, r)

	var entry *cacheEntry
	if rec.status == http.StatusOK {
		entry = &cacheEntry{
			body:        rec.body,
			contentType: rec.header.Get("Content-Type"),
			expiresAt:   time.Now().Add(c.ttl),
		}
	}

	c.mu.Lock()
	if entry != nil {
		if len(c.entries) >= c.maxKeys {
			c.entries = make(map[string]*cacheEntry, c.maxKeys)
		}
		c.entries[key] = entry
	}
	call.entry = entry
	delete(c.inflight, key)
	c.mu.Unlock()
	close(call.done)
	if entry == nil {
		return nil, false, rec
	}
	return entry, false, nil
}

// cacheKey 는 경로와 정렬된 쿼리 파라미터로 키를 만든다. 파라미터 순서가 달라도 같은 요청이면 같은
// 키가 되도록 정렬하고, at 을 포함한 전 파라미터를 키에 싣는다.
func cacheKey(r *http.Request) string {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(r.URL.Path)
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			b.WriteString("\x00")
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(v)
		}
	}
	return b.String()
}

// writeCached 는 저장된 응답을 내보낸다. X-Cache 헤더로 운영자가 캐시 적중을 확인한다.
func writeCached(w http.ResponseWriter, e *cacheEntry, status string) {
	if e.contentType != "" {
		w.Header().Set("Content-Type", e.contentType)
	}
	w.Header().Set("X-Cache", status)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.body)
}

// cacheRecorder 는 핸들러 응답을 메모리에 담는 ResponseWriter 다. 캐시 대상 응답은 수 MB 수준의
// JSON 이라 버퍼링이 안전하다 (limit/offset 파라미터로 상한을 줄 수 있고, 응답 자체가 이미 메모리에
// marshal 된 뒤 Write 된다).
type cacheRecorder struct {
	header http.Header
	status int
	body   []byte
}

func (cr *cacheRecorder) Header() http.Header { return cr.header }

func (cr *cacheRecorder) WriteHeader(code int) { cr.status = code }

func (cr *cacheRecorder) Write(p []byte) (int, error) {
	cr.body = append(cr.body, p...)
	return len(p), nil
}

// flushTo 는 담아 둔 응답을 실제 ResponseWriter 로 옮긴다. 캐시하지 않는 응답 (비 200) 전달 경로다.
func (cr *cacheRecorder) flushTo(w http.ResponseWriter) {
	for k, vs := range cr.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(cr.status)
	_, _ = w.Write(cr.body)
}
