package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// MaxSnapshotResponseBytes 는 correlation-exporter /snapshot 응답의 상한이다. Top-N 기본 10 *
// dimension 4 * cluster 활성 victim 수백 단위라 일반 운영에서는 수십 KB 이하이며, 1 MiB 한도
// 는 비정상 대용량 응답이 본 프로세스 메모리를 점유하는 케이스를 방어한다.
const MaxSnapshotResponseBytes = 1 << 20

// httpSnapshotSource 는 correlation-exporter 의 /snapshot endpoint 에서 NoisyNeighbor 리스트를
// HTTP GET 으로 가져와 staleCache 에 저장한다. cache hit 시 HTTP 호출을 건너뛰어 webhook 응답
// 시간을 단축하고, miss 시에도 짧은 fetchTimeout 으로 webhook 임계 전체를 한 호출이 잠식하지
// 못하게 한다.
type httpSnapshotSource struct {
	url     string
	client  *http.Client
	timeout time.Duration
	cache   *staleCache
}

func newHTTPSnapshotSource(url string, fetchTimeout, ttl time.Duration) *httpSnapshotSource {
	return &httpSnapshotSource{
		url:     url,
		client:  &http.Client{Timeout: fetchTimeout},
		timeout: fetchTimeout,
		cache:   newStaleCache(ttl),
	}
}

// fetch 는 cache fresh 면 즉시 반환, cache stale 또는 miss 면 HTTP 호출 후 cache 갱신한다.
// HTTP 실패 시에는 cache 의 stale 값을 그대로 돌려준다 (fallback). cache 가 비어 있고 HTTP 도
// 실패하면 빈 슬라이스를 돌려준다.
func (s *httpSnapshotSource) fetch(ctx context.Context) []snapshotEntry {
	if cached, fresh := s.cache.get(); fresh {
		return cached
	}

	// #419 요청 컨텍스트 파생. 클라이언트 취소 / 서버 타임아웃이 하류 fetch 까지 전파되고,
	// timeout 은 남은 요청 예산과 per-fetch 상한 중 먼저 오는 쪽이 걸린다.
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	entries, err := s.doFetch(ctx)
	if err != nil {
		log.Printf("snapshot fetch %s: %v", s.url, err)
		// stale 값이라도 있으면 그대로 반환 (fallback).
		if cached, _ := s.cache.get(); cached != nil {
			return cached
		}
		return nil
	}

	s.cache.store(entries)
	return entries
}

// probe 는 readiness 용 connectivity 검사다. doFetch 와 달리 최대 1 MiB JSON 을 다운로드 / 디코드
// 하지 않고 HTTP GET 후 200 status 만 확인 해 경량 으로 연결성 만 본다. cache 도 우회 한다.
func (s *httpSnapshotSource) probe(ctx context.Context) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("snapshot source not initialized")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return nil
}

// doFetch 는 HTTP GET 한 번 수행 후 응답 본문을 snapshotEntry 슬라이스로 unmarshal 한다.
// correlation.NoisyNeighbor 의 JSON tag 가 nested PodIdentity 라 본 패키지의 평면 struct 와
// shape 가 달라 중간 typed unmarshal struct 를 사용한다.
func (s *httpSnapshotSource) doFetch(ctx context.Context) ([]snapshotEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	// correlation NoisyNeighbor wire format (PodIdentity nested) 를 매핑한다.
	type wirePodIdentity struct {
		Namespace string `json:"namespace"`
		Pod       string `json:"pod"`
		PodUID    string `json:"pod_uid"`
	}
	type wireNeighbor struct {
		Victim    wirePodIdentity `json:"victim"`
		Suspect   wirePodIdentity `json:"suspect"`
		Dimension string          `json:"dimension"`
		Score     float64         `json:"score"`
	}

	var raw []wireNeighbor
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxSnapshotResponseBytes)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	out := make([]snapshotEntry, 0, len(raw))
	for _, r := range raw {
		out = append(out, snapshotEntry{
			VictimNamespace:  r.Victim.Namespace,
			VictimPod:        r.Victim.Pod,
			SuspectNamespace: r.Suspect.Namespace,
			SuspectPod:       r.Suspect.Pod,
			Dimension:        r.Dimension,
			Score:            r.Score,
		})
	}
	return out, nil
}
