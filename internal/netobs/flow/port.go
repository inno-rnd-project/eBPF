package flow

import "strconv"

// uint16ToString 은 strconv.FormatUint wrapper 다. formatPort 호출부 가 strconv import 를 직접 들고
// 다니지 않도록 helper 한 곳 에 모아 둔다.
func uint16ToString(p uint16) string {
	return strconv.FormatUint(uint64(p), 10)
}
