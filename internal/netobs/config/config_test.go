package config

import (
	"reflect"
	"testing"
)

// TestParseNamespaceList는 콤마 구분 namespace 입력의 정규화 (trim / 빈 토큰 제거 / 중복 dedup)
// 가 의도대로 동작하는지 검증한다. env/CLI 두 surface가 동일 정규화를 거치므로 본 함수가 운영자
// 입력 surface의 단일 진입점이며, 회귀가 발생하면 dst_pod_uid allow-list 게이트가 의도 외 동작
// 한다.
func TestParseNamespaceList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "whitespace_only", in: "  ,  ,", want: nil},
		{name: "single", in: "ebpf-project", want: []string{"ebpf-project"}},
		{name: "multi_with_spaces", in: " ns-a , ns-b ,ns-c ", want: []string{"ns-a", "ns-b", "ns-c"}},
		{name: "dedup_preserves_first_order", in: "ns-a,ns-b,ns-a,ns-c,ns-b", want: []string{"ns-a", "ns-b", "ns-c"}},
		{name: "drops_empty_tokens", in: ",ns-a,,ns-b,", want: []string{"ns-a", "ns-b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseNamespaceList(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseNamespaceList(%q)=%v want %v", c.in, got, c.want)
			}
		})
	}
}
