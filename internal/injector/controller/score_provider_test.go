package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCorrelationScoreClient_MaxScore 는 client 가 server-side 필터 파라미터 를 전달 하고 응답 항목
// 중 최대 score 를 반환 하는지 검증 한다.
func TestCorrelationScoreClient_MaxScore(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"score":0.42},{"score":0.81},{"score":0.6}],"page":{}}`))
	}))
	defer srv.Close()

	c := NewCorrelationScoreClient(srv.URL)
	score, found, err := c.MaxScore(context.Background(),
		PodRef{Namespace: "default", Pod: "victim"},
		PodRef{Namespace: "ebpf-project", Pod: "suspect"},
		"cpu")
	if err != nil {
		t.Fatalf("MaxScore: %v", err)
	}
	if !found {
		t.Fatal("found=false, want true")
	}
	if score != 0.81 {
		t.Errorf("score=%v want 0.81 (최대)", score)
	}
	for _, want := range []string{"victim_namespace=default", "victim_pod=victim", "suspect_namespace=ebpf-project", "suspect_pod=suspect", "dimension=cpu", "limit=1000"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query=%q 에 %q 없음", gotQuery, want)
		}
	}
}

// TestCorrelationScoreClient_NoMatch 는 매칭 페어 가 없을 때 (0, false, nil) 로 에러 와 구분 하는지
// 검증 한다.
func TestCorrelationScoreClient_NoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"page":{}}`))
	}))
	defer srv.Close()
	c := NewCorrelationScoreClient(srv.URL)
	score, found, err := c.MaxScore(context.Background(), PodRef{Pod: "v"}, PodRef{}, "")
	if err != nil {
		t.Fatalf("MaxScore: %v", err)
	}
	if found || score != 0 {
		t.Errorf("found=%v score=%v want false/0", found, score)
	}
}

// TestCorrelationScoreClient_EmptySuspectOmitsParam 은 suspect.Pod 가 비면 suspect 필터 파라미터 를
// 생략 해 victim 의 모든 suspect 를 대상 으로 하는지 검증 한다.
func TestCorrelationScoreClient_EmptySuspectOmitsParam(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"items":[{"score":0.5}],"page":{}}`))
	}))
	defer srv.Close()
	c := NewCorrelationScoreClient(srv.URL)
	if _, _, err := c.MaxScore(context.Background(), PodRef{Pod: "v"}, PodRef{}, ""); err != nil {
		t.Fatalf("MaxScore: %v", err)
	}
	if strings.Contains(gotQuery, "suspect_pod") || strings.Contains(gotQuery, "dimension=") {
		t.Errorf("query=%q 에 빈 suspect/dimension 파라미터 가 포함됨", gotQuery)
	}
}

// TestCorrelationScoreClient_HTTPError 는 non-200 응답 을 에러 로 분류 하는지 검증 한다.
func TestCorrelationScoreClient_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := NewCorrelationScoreClient(srv.URL)
	if _, _, err := c.MaxScore(context.Background(), PodRef{Pod: "v"}, PodRef{}, ""); err == nil {
		t.Error("non-200 인데 err=nil (에러 기대)")
	}
}
