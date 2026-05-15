package config

import (
	"reflect"
	"testing"
)

// TestGetenvFloat은 env 값의 float 파싱이 빈 값에서 default, 유효 값에서 그 값, 잘못된 입력에서
// 에러를 반환하는지 검증한다. NIC capacity tunable 의 env 진입점이 startup 시 fail-fast 함을 가드.
func TestGetenvFloat(t *testing.T) {
	const key = "TEST_GETENV_FLOAT_KEY"
	t.Run("empty_returns_default", func(t *testing.T) {
		t.Setenv(key, "")
		v, err := getenvFloat(key, 1.25e9)
		if err != nil || v != 1.25e9 {
			t.Errorf("empty env: got (%v, %v) want (1.25e9, nil)", v, err)
		}
	})
	t.Run("valid_returns_parsed", func(t *testing.T) {
		t.Setenv(key, "2.5e9")
		v, err := getenvFloat(key, 1.25e9)
		if err != nil || v != 2.5e9 {
			t.Errorf("valid env: got (%v, %v) want (2.5e9, nil)", v, err)
		}
	})
	t.Run("invalid_returns_error", func(t *testing.T) {
		t.Setenv(key, "not-a-number")
		_, err := getenvFloat(key, 1.25e9)
		if err == nil {
			t.Errorf("invalid env: err=nil want non-nil")
		}
	})
}

// TestValidateNamespaceName은 RFC1123 DNS 라벨 규칙 위반이 명시적 에러로 잡히는지 검증한다.
// 운영자가 흔히 저지르는 오타 (대문자, 언더스코어, 공백) 와 길이 초과를 startup 시점에 fail-fast
// 로 차단해 silent miss 디버깅 시간을 줄인다.
func TestValidateNamespaceName(t *testing.T) {
	valid := []string{"ebpf-project", "kube-system", "a", "x-1", "ns-with-numbers-123"}
	for _, ns := range valid {
		if err := validateNamespaceName(ns); err != nil {
			t.Errorf("validateNamespaceName(%q) err=%v want nil", ns, err)
		}
	}

	invalid := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"uppercase", "KubeSystem"},
		{"underscore", "kube_system"},
		{"leading_hyphen", "-foo"},
		{"trailing_hyphen", "foo-"},
		{"whitespace", "foo bar"},
		{"too_long", string(make([]byte, 64))},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateNamespaceName(tc.in); err == nil {
				t.Errorf("validateNamespaceName(%q) err=nil want non-nil", tc.in)
			}
		})
	}
}

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
