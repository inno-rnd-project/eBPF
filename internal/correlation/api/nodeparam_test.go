package api

import (
	"strings"
	"testing"
)

// TestParseNodeParam 은 #257 의 node 검증 헬퍼가 DNS-1123 형식만 통과시키고 빈 값을 전체 노드로
// 취급하는지 검증한다.
func TestParseNodeParam(t *testing.T) {
	valid := []string{"", "gpu", "ebpf-worker1", "node.example.com", "a", "gpu-01"}
	for _, v := range valid {
		if got, err := parseNodeParam(v); err != nil {
			t.Errorf("parseNodeParam(%q) err=%v want nil", v, err)
		} else if got != v {
			t.Errorf("parseNodeParam(%q)=%q want %q", v, got, v)
		}
	}
	invalid := []string{
		`gpu"} or up{`,           // PromQL injection 시도
		"UPPER",                  // 대문자
		"node;drop",              // 세미콜론
		"a/b",                    // 슬래시
		"-lead",                  // 하이픈 시작
		"trail-",                 // 하이픈 끝
		"a b",                    // 공백
		strings.Repeat("a", 254), // 길이 초과
	}
	for _, v := range invalid {
		if _, err := parseNodeParam(v); err == nil {
			t.Errorf("parseNodeParam(%q) err=nil want 거부", v)
		}
	}
}

// TestParseNamespaceParam 은 #409 의 namespace 공통 검증을 테이블로 고정한다.
func TestParseNamespaceParam(t *testing.T) {
	valid := []string{"", "default", "kube-system", "a", "ns-1", "monitoring"}
	for _, in := range valid {
		if _, err := parseNamespaceParam(in); err != nil {
			t.Errorf("parseNamespaceParam(%q) err=%v want nil", in, err)
		}
	}
	invalid := []string{
		"UPPER", "has_underscore", "has.dot", "-lead", "trail-",
		`evil"} or up{`, "line\nbreak", strings.Repeat("a", 64),
	}
	for _, in := range invalid {
		if _, err := parseNamespaceParam(in); err == nil {
			t.Errorf("parseNamespaceParam(%q) err=nil want 거부", in)
		}
	}
}
