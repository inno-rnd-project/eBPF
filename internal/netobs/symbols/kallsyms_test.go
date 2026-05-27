package symbols

import (
	"os"
	"path/filepath"
	"testing"
)

// writeKallsyms 는 테스트 용 kallsyms 파일 을 임시 디렉터리 에 작성한다. real /proc/kallsyms 의 한 줄
// 포맷 "<addr> <type> <name>" 를 그대로 따른다.
func writeKallsyms(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kallsyms")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write kallsyms fixture: %v", err)
	}
	return path
}

// TestLoadKallsyms_BasicSorting 은 kallsyms 입력 이 주소 순서 와 무관 하게 들어 와도 내부 sorted slice
// 로 정렬 되고 _text 심볼 이 base 로 추출 되는지 검증한다.
func TestLoadKallsyms_BasicSorting(t *testing.T) {
	content := `ffffffffa8000000 T _text
ffffffffa8d7fd30 T kfree_skb_reason
ffffffffa8d7fa00 T kfree_skb
ffffffffa8eac3c0 T tcp_v4_rcv
`
	path := writeKallsyms(t, content)
	tbl, err := loadKallsyms(path)
	if err != nil {
		t.Fatalf("loadKallsyms: %v", err)
	}
	if tbl.base != 0xffffffffa8000000 {
		t.Errorf("base=%x want ffffffffa8000000", tbl.base)
	}
	// sort 후 첫 항목 의 주소 가 가장 작은 것 (_text) 이어야 한다.
	if tbl.syms[0].addr != 0xffffffffa8000000 {
		t.Errorf("sorted[0].addr=%x want _text", tbl.syms[0].addr)
	}
}

// TestLoadKallsyms_AllZeroFails 는 kptr_restrict 로 주소 가 전부 0 마스킹 된 케이스 에서 loadKallsyms
// 가 에러 를 반환 해 호출자 가 fail-open 분기 를 타게 하는 회귀 가드 다.
func TestLoadKallsyms_AllZeroFails(t *testing.T) {
	content := `0000000000000000 T _text
0000000000000000 T kfree_skb_reason
0000000000000000 T tcp_v4_rcv
`
	path := writeKallsyms(t, content)
	if _, err := loadKallsyms(path); err == nil {
		t.Errorf("loadKallsyms 가 all-zero 입력 에서 성공함 (kptr_restrict 가드 미동작)")
	}
}

// TestResolve_AddressLookup 은 binary search 가 target 이하 의 가장 가까운 symbol 을 정확히 찾는지
// 검증한다. target 이 첫 symbol 보다 작으면 빈 문자열 을 반환 해 호출자 가 frame 을 skip 하게 한다.
func TestResolve_AddressLookup(t *testing.T) {
	content := `ffffffffa8000000 T _text
ffffffffa8d7fa00 T kfree_skb
ffffffffa8d7fd30 T kfree_skb_reason
ffffffffa8eac3c0 T tcp_v4_rcv
`
	path := writeKallsyms(t, content)
	tbl, err := loadKallsyms(path)
	if err != nil {
		t.Fatalf("loadKallsyms: %v", err)
	}

	cases := []struct {
		ip   uint64
		want string
	}{
		{0xffffffffa8d7fd30, "kfree_skb_reason"}, // exact match
		{0xffffffffa8d7fd50, "kfree_skb_reason"}, // mid-function offset
		{0xffffffffa8eac3c0, "tcp_v4_rcv"},       // exact next symbol
		{0xffffffffa7000000, ""},                 // before _text
		{0xffffffffa8d7fa01, "kfree_skb"},        // mid kfree_skb
	}
	for _, tc := range cases {
		got := tbl.resolve(tc.ip)
		if got != tc.want {
			t.Errorf("resolve(%x)=%q want %q", tc.ip, got, tc.want)
		}
	}
}
