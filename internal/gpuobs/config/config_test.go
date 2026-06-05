package config

import (
	"reflect"
	"testing"
)

// TestParseNamespaceList 는 #104 allow-list env 파싱 의 4 분기 (empty / single / multi / 중복 + 공백) 검증.
func TestParseNamespaceList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace_only", "   ", nil},
		{"single", "ml", []string{"ml"}},
		{"multi", "ml,infra,kube-system", []string{"ml", "infra", "kube-system"}},
		{"trim_and_dedupe", " ml , ml , infra ,, ", []string{"ml", "infra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNamespaceList(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseNamespaceList(%q)=%v want %v", tc.in, got, tc.want)
			}
		})
	}
}
