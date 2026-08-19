package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func playbooksGet(t *testing.T, target string) (*httptest.ResponseRecorder, PlaybooksResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	NewPlaybooksHandler().GetPlaybooks(rec, httptest.NewRequest(http.MethodGet, target, nil))
	var resp PlaybooksResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec, resp
}

// TestPlaybooks_Catalog 는 전체 카탈로그가 4 kind 를 모두 포함하고 각 항목이 확인 절차와 권고
// 조치를 갖추는지 검증한다.
func TestPlaybooks_Catalog(t *testing.T) {
	rec, resp := playbooksGet(t, "/api/v1/playbooks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if len(resp.Playbooks) != len(playbookCatalog) {
		t.Fatalf("playbooks=%d want %d", len(resp.Playbooks), len(playbookCatalog))
	}
	kinds := map[string]int{}
	for _, p := range resp.Playbooks {
		kinds[p.Kind]++
		if p.Title == "" || p.Description == "" || len(p.Checks) == 0 || len(p.Actions) == 0 {
			t.Errorf("항목 %s 불완전: %+v", p.Cause, p)
		}
		for _, c := range p.Checks {
			if !strings.HasPrefix(c.API, "/api/v1/") {
				t.Errorf("항목 %s 의 check api=%q 가 /api/v1/ 경로 아님", p.Cause, c.API)
			}
		}
	}
	for kind, want := range map[string]int{"gpu_idle_cause": 9, "drop_stage": 10, "dimension": 4, "alert": 9} {
		if kinds[kind] != want {
			t.Errorf("kind %s=%d want %d", kind, kinds[kind], want)
		}
	}
}

// TestPlaybooks_CauseLookup 은 단일 조회가 정식 식별자와 alias (alertname) 양쪽으로 매칭되는지
// 검증한다. registry 등록 alert 11종이 모두 어느 항목으로든 조회 가능해야 한다.
func TestPlaybooks_CauseLookup(t *testing.T) {
	rec, resp := playbooksGet(t, "/api/v1/playbooks?cause=network_pressure")
	if rec.Code != http.StatusOK || len(resp.Playbooks) != 1 || resp.Playbooks[0].Cause != "network_pressure" {
		t.Fatalf("code=%d playbooks=%+v want network_pressure 단건", rec.Code, resp.Playbooks)
	}
	// alias 매칭: alertname 으로 조회해도 동일 항목.
	rec, resp = playbooksGet(t, "/api/v1/playbooks?cause=GPUIdleWithNetworkPressure")
	if rec.Code != http.StatusOK || len(resp.Playbooks) != 1 || resp.Playbooks[0].Cause != "network_pressure" {
		t.Fatalf("alias 조회 실패: code=%d playbooks=%+v", rec.Code, resp.Playbooks)
	}
	// registry 등록 alert 11종 전수가 조회 가능해야 한다.
	for _, alert := range []string{
		"GPUIdleWithPCIeSaturation", "GPUIdleWithNetworkPressure", "GPUIdleWithCPUThrottle",
		"GPUIdleWithMemoryPressure", "GPUIdleWithHostComputeStall", "GPUIdleWithDCGMPCIeReplay",
		"GPUIdleWithNCCLCollectiveStall", "GPUObsCudaStreamWaitHigh", "GPUObsThermalThrottleSustained",
		"NetObsDropBurst", "CorrelationStrongNoisyNeighbor",
	} {
		if findPlaybook(alert) == nil {
			t.Errorf("registry alert %s 가 카탈로그에서 조회 불가", alert)
		}
	}
}

// TestPlaybooks_UnknownCause 는 미등록 식별자가 404 로 구분되는지 검증한다.
func TestPlaybooks_UnknownCause(t *testing.T) {
	rec, _ := playbooksGet(t, "/api/v1/playbooks?cause=bogus")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", rec.Code)
	}
}

// TestPlaybooks_AtParam 은 at 지정 시 시점 조회 지원 경로에만 at 이 붙는지와 잘못된 at 의 400 을
// 검증한다. noisy-neighbor 처럼 at 미지원 경로는 원본 그대로여야 한다.
func TestPlaybooks_AtParam(t *testing.T) {
	rec, resp := playbooksGet(t, "/api/v1/playbooks?cause=cpu_throttle&at=2026-07-08T03:00:00Z")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var withAtCount, plainNeighbor int
	for _, c := range resp.Playbooks[0].Checks {
		switch {
		case strings.Contains(c.API, "at=2026-07-08T03:00:00Z"):
			withAtCount++
		case strings.HasPrefix(c.API, "/api/v1/noisy-neighbor") && !strings.Contains(c.API, "at="):
			plainNeighbor++
		}
	}
	if withAtCount == 0 || plainNeighbor == 0 {
		t.Errorf("checks=%+v want at 부착 (지원 경로) 과 미부착 (noisy-neighbor) 공존", resp.Playbooks[0].Checks)
	}
	// 기존 쿼리 파라미터가 있는 경로는 & 로 이어 붙는다.
	rec, resp = playbooksGet(t, "/api/v1/playbooks?cause=cpu&at=1751943600")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	found := false
	for _, c := range resp.Playbooks[0].Checks {
		if strings.Contains(c.API, "dimension=cpu&scope=pod&at=") {
			found = true
		}
	}
	if !found {
		t.Errorf("checks=%+v want 기존 파라미터 뒤 & 연결", resp.Playbooks[0].Checks)
	}
	rec, _ = playbooksGet(t, "/api/v1/playbooks?at=bogus")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want 400 (invalid at)", rec.Code)
	}
}

// TestPlaybooks_UniqueIdentifiers 는 cause 와 alias 를 통틀어 식별자 충돌이 없는지 검증한다.
// 충돌하면 단일 조회가 선언 순서에 따라 임의 항목을 돌려주게 된다.
func TestPlaybooks_UniqueIdentifiers(t *testing.T) {
	seen := map[string]string{}
	for _, e := range playbookCatalog {
		ids := append([]string{e.cause}, e.aliases...)
		for _, id := range ids {
			if prev, dup := seen[id]; dup {
				t.Errorf("식별자 %q 충돌: %s 와 %s", id, prev, e.cause)
			}
			seen[id] = e.cause
		}
	}
}
