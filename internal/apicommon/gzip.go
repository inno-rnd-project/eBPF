package apicommon

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

// gzipWriterPool 은 gzip.Writer 재사용 풀이다. 폴링 경로에서 요청마다 writer 를 새로 만들면 압축
// 사전 버퍼 할당이 반복되므로 재사용한다.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// GzipMiddleware 는 #411 의 응답 압축이다. 클러스터 규모에 정비례하는 목록 응답 (/pods, /nodes,
// /node-map) 은 반복 라벨이 많은 JSON 이라 압축률이 높아 전송량과 프론트 수신 지연이 함께 줄어든다.
// Accept-Encoding 에 gzip 이 없는 소비자에는 종전과 동일한 평문을 돌려주므로 호환이 유지된다.
//
// 미들웨어 순서는 캐시 안쪽 (핸들러에 가까운 쪽) 이 아니라 캐시 바깥이다. 캐시가 평문을 저장해야
// Accept-Encoding 이 다른 소비자가 같은 캐시 항목을 공유할 수 있고, 압축은 요청별로 수행된다.
func GzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		defer func() {
			_ = gz.Close()
			gzipWriterPool.Put(gz)
		}()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
	})
}

// gzipResponseWriter 는 본문만 gzip 으로 흘리는 wrapper 다. Content-Length 는 압축 후 값이 달라지므로
// 제거하고 chunked 로 내보낸다.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.ResponseWriter.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	return g.gz.Write(p)
}
