package api

import (
	"fmt"
	"regexp"
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
