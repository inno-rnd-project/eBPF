package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// nodeNamePattern 은 Kubernetes 노드 이름 (kubernetes.io/hostname) 의 DNS-1123 subdomain 형식이다.
// 소문자 영숫자로 시작·끝나고 중간에 - 와 . 를 허용하며 라벨당 최대 63, 전체 최대 253 자다. 본 패턴은
// 길이 상한만 아래 parseNodeParam 에서 별도 검사하고 문자 구성만 검증한다.
var nodeNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// parseNodeParam 은 #257 의 node 쿼리 파라미터 공용 검증 헬퍼다. node-scoped 엔드포인트가 사용자
// 입력 노드 이름을 PromQL 에 결합하기 전에 DNS-1123 형식으로 사전 검증해, exact = 매처와 %q 결합
// 전제 (셸/PromQL 메타문자 부재) 를 입력 경계에서 보장한다. 빈 문자열은 "전체 노드" 를 뜻해 (ok=true,
// name="") 로 통과시키고, 형식 위반만 거부한다. 검증을 통과한 값은 =~ 정규식 매처에 넣지 않아 전체
// 스캔과 ReDoS 표면을 만들지 않는다.
func parseNodeParam(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > 253 {
		return "", fmt.Errorf("node 이름이 253 자를 초과합니다")
	}
	if !nodeNamePattern.MatchString(raw) {
		return "", fmt.Errorf("node 이름이 DNS-1123 형식이 아닙니다: %q", raw)
	}
	return raw, nil
}

// namespacePattern 은 Kubernetes namespace 의 DNS-1123 label 형식이다. subdomain (node 이름) 과
// 달리 점을 허용하지 않는다.
var namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// parseNamespaceParam 은 #409 의 namespace 쿼리 파라미터 공통 검증이다. 빈 값은 필터 미적용 의미
// 그대로 통과시키고, 값이 있으면 DNS-1123 label (63자 이하) 형식을 강제한다. 검증 통과 값은 PromQL
// %q 결합과 Go 측 비교 어느 쪽에 쓰여도 안전하다는 단일 정책의 진입점이며, 종전에는 6개 핸들러
// 어디서도 검증이 없어 임의 문자열이 쿼리와 응답과 로그에 반사됐다.
func parseNamespaceParam(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if len(raw) > 63 {
		return "", fmt.Errorf("namespace 가 63 자를 초과합니다")
	}
	if !namespacePattern.MatchString(raw) {
		return "", fmt.Errorf("namespace 가 DNS-1123 label 형식이 아닙니다: %q", raw)
	}
	return raw, nil
}

// nodeMatcher 는 검증된 node 로 PromQL exact matcher 조각 (node="...") 을 만든다. node 가 비면 빈
// 문자열을 돌려줘 promSelector 에서 자연 제외된다. 값은 parseNodeParam 검증을 통과한 DNS-1123 문자열
// 이라 %q 결합이 안전하다.
func nodeMatcher(node string) string {
	if node == "" {
		return ""
	}
	return fmt.Sprintf("node=%q", node)
}

// promSelector 는 빈 조각을 제외한 matcher 들을 PromQL label selector `{...}` 로 조립한다. 조각이
// 모두 비면 빈 문자열을 돌려줘 호출부가 bare metric (selector 없는 형태) 을 그대로 유지하게 한다.
// node-scoped 필터가 기존 namespace 등 다른 matcher 와 하나의 selector 로 병합될 때 쓴다.
func promSelector(matchers ...string) string {
	nz := make([]string, 0, len(matchers))
	for _, m := range matchers {
		if m != "" {
			nz = append(nz, m)
		}
	}
	if len(nz) == 0 {
		return ""
	}
	return "{" + strings.Join(nz, ",") + "}"
}

// parsePageParams 는 #411 의 opt-in 목록 상한이다. limit 과 offset 이 모두 미지정이면 (0, 0, false)
// 를 돌려줘 호출부가 종전처럼 전량을 노출하고, 하나라도 지정되면 그 값으로 슬라이스를 자른다.
// 클러스터 규모에 정비례하던 응답 (/pods, /nodes, /node-map) 에 소비자가 상한을 걸 수 있게 하되
// 기본 동작은 바꾸지 않아 기존 소비자에 무영향이다. limit 상한은 목록 응답의 실질 최대치 (노드 수천,
// pod 수만) 를 덮는 10000 이고, 형식 위반은 400 으로 거부한다.
func parsePageParams(r *http.Request) (limit, offset int, paged bool, err error) {
	q := r.URL.Query()
	rawLimit := strings.TrimSpace(q.Get("limit"))
	rawOffset := strings.TrimSpace(q.Get("offset"))
	if rawLimit == "" && rawOffset == "" {
		return 0, 0, false, nil
	}
	if rawLimit != "" {
		v, convErr := strconv.Atoi(rawLimit)
		if convErr != nil || v <= 0 {
			return 0, 0, false, fmt.Errorf("limit 은 양의 정수여야 합니다")
		}
		if v > 10000 {
			v = 10000
		}
		limit = v
	}
	if rawOffset != "" {
		v, convErr := strconv.Atoi(rawOffset)
		if convErr != nil || v < 0 {
			return 0, 0, false, fmt.Errorf("offset 은 0 이상 정수여야 합니다")
		}
		offset = v
	}
	if limit == 0 {
		limit = 10000
	}
	return limit, offset, true, nil
}

// pageSlice 는 total 개 항목에서 offset 부터 limit 개의 인덱스 구간을 돌려준다. 범위를 벗어난 offset
// 은 빈 구간이 되어 호출부가 빈 목록을 응답한다.
func pageSlice(total, limit, offset int) (start, end int) {
	if offset >= total {
		return total, total
	}
	start = offset
	end = offset + limit
	if end > total {
		end = total
	}
	return start, end
}
