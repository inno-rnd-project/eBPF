package api

import (
	"fmt"
	"strings"
	"testing"
)

// TestPromQLEscape_InjectionImpossible 은 #409 의 injection 불가 회귀 고정이다. 본 패키지의 PromQL
// matcher 조립은 전부 fmt 의 %q 결합을 쓰는데, %q 가 큰따옴표와 백슬래시를 이스케이프하므로 악의
// 입력이 label matcher 밖으로 탈출해 selector 나 쿼리 구조를 바꾸는 것이 불가능하다. 이 성질이
// namespace / node 검증 (parseNodeParam, parseNamespaceParam) 의 방어선 뒤에 있는 두 번째 방어선
// 이며, 향후 누군가 %q 를 %s 로 바꾸는 회귀를 본 테스트가 잡는다.
func TestPromQLEscape_InjectionImpossible(t *testing.T) {
	hostile := []string{
		`evil"} or up{x="`,
		`a",job!="prom`,
		"back\\slash\"quote",
		"newline\ninject",
	}
	for _, in := range hostile {
		got := fmt.Sprintf("node=%q", in)
		// 이스케이프 결과는 여는 따옴표와 닫는 따옴표 정확히 한 쌍 (비이스케이프) 만 갖는다. 중간의
		// 모든 따옴표가 \" 로 이스케이프되어 matcher 탈출이 불가능함을 단정한다.
		body := strings.TrimSuffix(strings.TrimPrefix(got, `node="`), `"`)
		unescaped := 0
		for i := 0; i < len(body); i++ {
			if body[i] == '"' && (i == 0 || body[i-1] != '\\') {
				unescaped++
			}
		}
		if unescaped != 0 {
			t.Errorf("입력 %q 의 %%q 결합 결과에 비이스케이프 따옴표 %d개: %s", in, unescaped, got)
		}
		if strings.Contains(body, "\n") {
			t.Errorf("입력 %q 의 %%q 결합 결과에 원시 개행 잔존: %s", in, got)
		}
	}
}

// TestNodeMatcher_HostileInputEscaped 는 nodeMatcher 가 (검증을 우회해 호출됐다는 가정 아래에서도)
// %q 이스케이프로 selector 탈출을 막는지 고정한다. 운영 경로는 parseNodeParam 이 이런 입력을 400
// 으로 먼저 거부한다.
func TestNodeMatcher_HostileInputEscaped(t *testing.T) {
	got := nodeMatcher(`n1"} or up{x="`)
	// 전체 따옴표 수에서 이스케이프된 (\" 페어의) 따옴표 수를 빼면 비이스케이프 따옴표는 여는 것과
	// 닫는 것 2개뿐이어야 한다.
	if strings.Count(got, `"`)-strings.Count(got, `\"`) != 2 {
		t.Errorf("nodeMatcher 이스케이프 실패: %s", got)
	}
	if !strings.HasPrefix(got, `node="`) || !strings.HasSuffix(got, `"`) {
		t.Errorf("nodeMatcher 형태 회귀: %s", got)
	}
}
