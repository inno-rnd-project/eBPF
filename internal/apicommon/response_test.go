package apicommon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSON(w, map[string]string{"foo": "bar"})
	if w.Code != http.StatusOK {
		t.Errorf("status=%d want %d", w.Code, http.StatusOK)
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type=%q want application/json", w.Header().Get("Content-Type"))
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if body["foo"] != "bar" {
		t.Errorf("body[foo]=%q want bar", body["foo"])
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusBadRequest, "invalid_dimension", "dimension must be one of cpu/memory/network/gpu")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status=%d want %d", w.Code, http.StatusBadRequest)
	}
	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if body.Error.Code != "invalid_dimension" {
		t.Errorf("error.code=%q want invalid_dimension", body.Error.Code)
	}
}

func TestParsePagination(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantOffset int
	}{
		{"empty", "", PaginationDefaultLimit, 0},
		{"normal", "limit=50&offset=10", 50, 10},
		{"limit clamp upper", "limit=9999", PaginationMaxLimit, 0},
		{"limit zero defaults", "limit=0", PaginationDefaultLimit, 0},
		{"limit negative defaults", "limit=-5", PaginationDefaultLimit, 0},
		{"offset negative ignored", "offset=-3", PaginationDefaultLimit, 0},
		{"invalid limit ignored", "limit=abc", PaginationDefaultLimit, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			gotLimit, gotOffset := ParsePagination(r)
			if gotLimit != tt.wantLimit {
				t.Errorf("limit=%d want %d", gotLimit, tt.wantLimit)
			}
			if gotOffset != tt.wantOffset {
				t.Errorf("offset=%d want %d", gotOffset, tt.wantOffset)
			}
		})
	}
}

func TestApplyPagination(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	if got := ApplyPagination(items, 2, 1); !sliceEqual(got, []int{2, 3}) {
		t.Errorf("got=%v want [2,3]", got)
	}
	if got := ApplyPagination(items, 10, 0); !sliceEqual(got, items) {
		t.Errorf("got=%v want full slice", got)
	}
	if got := ApplyPagination(items, 5, 100); len(got) != 0 {
		t.Errorf("got=%v want empty", got)
	}
}

func sliceEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParseLimit 은 #352 의 limit 파싱 규약을 검증한다. 미지정은 def, 파싱 불가는 ok=false (400),
// 0 이하는 def, max 초과는 clamp.
func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw      string
		def, max int
		want     int
		wantOK   bool
	}{
		{"", 20, 100, 20, true},     // 미지정 → def
		{"50", 20, 100, 50, true},   // 정상
		{"0", 20, 100, 20, true},    // 0 → def
		{"-5", 20, 100, 20, true},   // 음수 → def
		{"500", 20, 100, 100, true}, // 초과 → clamp
		{"abc", 20, 100, 0, false},  // 파싱 불가 → 400
	}
	for _, tc := range cases {
		url := "/x"
		if tc.raw != "" {
			url += "?limit=" + tc.raw
		}
		r := httptest.NewRequest(http.MethodGet, url, nil)
		got, ok := ParseLimit(r, tc.def, tc.max)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ParseLimit(raw=%q)=%d,%v want %d,%v", tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}
