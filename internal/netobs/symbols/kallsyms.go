// Package symbols 는 #83 drop event stack capture 의 userspace symbol resolver 를 제공한다.
// /proc/kallsyms 를 startup 1 회 파싱해 sorted symbol table 을 구성하고 BPF stack_trace 맵의
// stack id 를 (top_function, stack_hash) 로 변환한다. resolver init 이 실패해도 nil 반환으로
// fail-open 정책을 따르고 호출자는 stack 메트릭 emit 만 skip 한다.
package symbols

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// symbol 은 kallsyms 한 항목의 (주소, 함수명) 쌍이다. base 는 _text 심볼 주소 로 KASLR offset 정규화
// 시 활용 가능한 진단 값이며 본 PR 의 in-memory cache 에서는 절대 주소 비교 만으로 lookup 한다.
type symbol struct {
	addr uint64
	name string
}

// kallsymsTable 은 정렬된 symbol slice 와 _text 기준 base 주소 를 보관한다. lookup 은 binary search
// 로 주소가 target IP 이하 인 가장 가까운 항목 을 찾고 본 항목 의 함수명 을 반환한다.
type kallsymsTable struct {
	syms []symbol
	base uint64
}

// loadKallsyms 는 /proc/kallsyms 를 파싱해 kallsymsTable 을 구성한다. kptr_restrict 정책 으로 주소
// 가 모두 0 으로 마스킹 된 경우 (CAP_SYSLOG 미보유 등) 빈 테이블 또는 의미 없는 0 주소 만 들어오므로
// 본 helper 는 nil 반환 으로 호출자 에게 resolver 비활성 을 알린다.
func loadKallsyms(path string) (*kallsymsTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open kallsyms %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	syms := make([]symbol, 0, 1<<16)
	var base uint64
	var nonZero int

	scanner := bufio.NewScanner(f)
	// kallsyms 한 줄 의 최대 길이 는 함수명 길이 에 따라 변동 가능하므로 default 64KiB buffer 를
	// 256KiB 로 확장 해 module 함수명 의 긴 demangling 도 안전 하게 처리한다.
	scanner.Buffer(make([]byte, 0, 4096), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		addr, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil {
			continue
		}
		name := fields[2]
		if addr != 0 {
			nonZero++
		}
		if name == "_text" {
			base = addr
		}
		syms = append(syms, symbol{addr: addr, name: name})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan kallsyms: %w", err)
	}
	if nonZero == 0 {
		// kptr_restrict 로 모든 주소 가 0 마스킹 된 케이스. resolver 가 의미 있는 frame 분석 을 할
		// 수 없으므로 명시적 에러 로 호출자 에게 알리고 fail-open 분기 를 타게 한다.
		return nil, fmt.Errorf("kallsyms 주소 가 전부 0 임 (kptr_restrict / CAP_SYSLOG 미보유 의심)")
	}

	sort.Slice(syms, func(i, j int) bool { return syms[i].addr < syms[j].addr })

	return &kallsymsTable{syms: syms, base: base}, nil
}

// resolve 는 IP 주소 에 해당 하는 함수명 을 binary search 로 찾는다. target 이하 의 가장 큰 주소 를
// 가진 symbol 의 이름 을 반환한다. table 이 비어 있거나 target 이 첫 symbol 보다 작 으면 빈 문자열 을
// 반환한다.
func (t *kallsymsTable) resolve(ip uint64) string {
	if len(t.syms) == 0 {
		return ""
	}
	// sort.Search 는 predicate true 인 첫 인덱스 를 반환 한다. ip < syms[i].addr 인 첫 i 를 찾고
	// 그 직전 인덱스 가 정답 이다.
	i := sort.Search(len(t.syms), func(i int) bool { return t.syms[i].addr > ip })
	if i == 0 {
		return ""
	}
	return t.syms[i-1].name
}
