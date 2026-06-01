// Package apicommon은 본 repo의 4 agent (netobs / gpuobs / correlation / rca-summarizer) 가
// 공통으로 사용하는 REST API 응답 표준과 pagination 표준 그리고 미들웨어를 모은 공용 패키지다.
// 이슈 #100의 자체 dashboard용 REST API layer 도입에서 신설되었으며 각 agent의 internal/<agent>/api
// 패키지가 본 표준을 차용한다.
package apicommon

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// PaginationMaxLimit은 단일 응답에서 반환 가능한 최대 item 개수다. 운영자가 큰 limit으로 cluster
// 자원을 과도 사용하지 않도록 hard cap을 둔다. 더 큰 데이터셋이 필요하면 offset 기반 분할 호출을
// 권장한다.
const PaginationMaxLimit = 1000

// PaginationDefaultLimit은 클라이언트가 limit 미지정 시 기본 적용되는 값이다. dashboard 페이지
// 단위 표시에 충분한 양으로 100을 채택한다.
const PaginationDefaultLimit = 100

// Page는 응답 메타데이터로 pagination 상태를 표현한다. limit과 offset은 요청 쿼리 파라미터와
// 정확히 일치하고 total은 필터 적용 후 전체 결과 개수다. 클라이언트는 (offset + limit) 가 total
// 이상이면 추가 페이지 없음으로 판단한다.
type Page struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// ListResponse는 모든 list형 endpoint의 표준 응답 형태다. Items는 endpoint별 도메인 객체의 slice
// 라 generic으로 두지 않고 json.RawMessage로 받아 직접 marshal한 결과를 끼워 넣는다.
type ListResponse struct {
	Items json.RawMessage `json:"items"`
	Page  Page            `json:"page"`
}

// ErrorBody는 4xx와 5xx 응답의 표준 형태다. code는 클라이언트가 분기 가능한 enum 문자열,
// message는 운영자가 읽는 자유 형식 설명이다.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail은 ErrorBody의 nested 구조다. code 와 message로 분리한다.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteJSON은 200 OK 응답에 JSON body를 기록한다. encode 실패는 로그만 남기고 client는 partial
// 응답을 받는다 (이미 헤더 전송 후 라 복구 불가).
func WriteJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("apicommon: json encode error: %v", err)
	}
}

// WriteError는 status 코드와 표준 ErrorBody를 응답한다. code 는 enum, message 는 운영자 메시지.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(ErrorBody{Error: ErrorDetail{Code: code, Message: message}}); err != nil {
		log.Printf("apicommon: json encode error: %v", err)
	}
}

// ParsePagination은 쿼리 파라미터에서 limit과 offset을 파싱한다. limit 음수와 0은 default로
// 대체되고 PaginationMaxLimit 초과는 clamp 된다. offset 음수는 0으로 보정된다.
func ParsePagination(r *http.Request) (limit, offset int) {
	limit = PaginationDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			if v <= 0 {
				limit = PaginationDefaultLimit
			} else if v > PaginationMaxLimit {
				limit = PaginationMaxLimit
			} else {
				limit = v
			}
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v >= 0 {
			offset = v
		}
	}
	return limit, offset
}

// ApplyPagination 은 slice에 limit과 offset을 적용해 잘라 반환한다. 호출 측은 결과를 그대로
// json.Marshal 하면 된다. 본 함수는 generic 으로 두어 4 agent 의 도메인 타입 어느 쪽이든 차용
// 가능하게 한다.
func ApplyPagination[T any](items []T, limit, offset int) []T {
	if offset >= len(items) {
		return []T{}
	}
	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	return items[offset:end]
}
